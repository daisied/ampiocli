// Package engine runs pull, diff and push over the domains, the parts of the
// M-SERV's state ampiocli manages (app configuration, module logic,
// Node-RED). The engine does the bookkeeping every domain shares: the
// working copy, conflict checks, backups and verification. A domain only
// knows how to read its state and how to write changes.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/daisied/ampiocli/internal/items"
	"github.com/daisied/ampiocli/internal/workdir"
)

// Domain is one part of the M-SERV's state.
type Domain interface {
	// Name is the domain's name on the command line.
	Name() string
	// Owns reports whether a working-copy file belongs to the domain.
	Owns(path string) bool
	// Pull reads the domain's live state as working-copy files. The engine
	// calls Prepare right after a Pull, so a domain may keep what the Pull
	// read for it (the module list, Node-RED's revision).
	Pull(ctx context.Context) (workdir.Files, error)
	// Items flattens the domain's files into items.
	Items(files workdir.Files) (items.Items, error)
	// Prepare checks and encodes changes before anything is written. It must
	// not write. live is what the Pull just before read and work the domain's
	// working-copy files, for the domains that need them.
	Prepare(changes []items.Change, live items.Items, work workdir.Files) (*Plan, error)
}

// Plan is a prepared write.
type Plan struct {
	Write func(ctx context.Context) error
	// Expect lists the changes as they must read back after the write, under
	// their final keys (new rows only get their ids in Prepare).
	Expect []items.Change
	// Work, if set, is the domain's working copy as it reads once written
	// (config gives new rows their ids). The engine saves it before writing,
	// so a push that fails part-way can be retried without writing anything twice.
	Work workdir.Files
	// After, if set, runs once the write is verified.
	After func(ctx context.Context) error
	// Notes are shown with the push's checks: what the write will do that
	// the diff does not say.
	Notes []string
}

// Engine works on one working copy. Domains are in write order.
type Engine struct {
	Copy    workdir.Copy
	Domains []Domain
	Out     io.Writer
}

// edits are one domain's unpushed edits.
type edits struct {
	domain  Domain
	base    items.Items
	work    workdir.Files
	changes []items.Change
}

func (e *Engine) unpushed() ([]edits, error) {
	var out []edits
	for _, d := range e.Domains {
		base, err := e.Copy.Base().Read(d.Owns)
		if err != nil {
			return nil, err
		}
		work, err := e.Copy.Work().Read(d.Owns)
		if err != nil {
			return nil, err
		}
		baseItems, err := d.Items(base)
		if err != nil {
			return nil, err
		}
		workItems, err := d.Items(work)
		if err != nil {
			return nil, err
		}
		if changes := items.Diff(baseItems, workItems); len(changes) > 0 {
			out = append(out, edits{d, baseItems, work, changes})
		}
	}
	return out, nil
}

// save records files as both the base and the work state of d.
func (e *Engine) save(d Domain, files workdir.Files) error {
	if err := e.Copy.Base().Write(files, d.Owns); err != nil {
		return err
	}
	return e.Copy.Work().Write(files, d.Owns)
}

// Pull replaces the working copy with the live state. Unpushed edits stop
// it unless force is set.
func (e *Engine) Pull(ctx context.Context, force bool) error {
	if err := e.Copy.Ignore(); err != nil {
		return err
	}
	if !force {
		all, err := e.unpushed()
		if err != nil {
			return err
		}
		if len(all) > 0 {
			return fmt.Errorf("unpushed edits in %s (see ampiocli diff); push them, or pull --force to discard them", names(all))
		}
	}
	for _, d := range e.Domains {
		files, err := d.Pull(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", d.Name(), err)
		}
		if err := e.save(d, files); err != nil {
			return err
		}
		fmt.Fprintf(e.Out, "%s: %d files\n", d.Name(), len(files))
	}
	return nil
}

// Diff prints the unpushed edits.
func (e *Engine) Diff() error {
	all, err := e.unpushed()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Fprintln(e.Out, "no edits")
	}
	for _, u := range all {
		e.show(u.domain.Name(), u.changes)
	}
	return nil
}

func (e *Engine) show(domain string, changes []items.Change) {
	fmt.Fprintf(e.Out, "%s: %d changed\n", domain, len(changes))
	for _, c := range changes {
		for _, line := range items.Describe(c) {
			fmt.Fprintln(e.Out, "  "+line)
		}
	}
}

// Push writes the unpushed edits. Without write it stops before writing,
// after every check has run.
//
// For each domain with edits, in order:
//  1. pull the live state and stop if an edited item changed since the last
//     pull; edits the live state already has (from a push that failed
//     part-way) are skipped
//  2. prepare the write, so a bad edit stops the push before anything is written
//  3. back up the live state of every domain, before the first write
//  4. check the live state again just before writing, then write, pull again
//     and check that every change reads back
//  5. make the new live state the base and work state
func (e *Engine) Push(ctx context.Context, write bool) error {
	all, err := e.unpushed()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Fprintln(e.Out, "nothing to push")
		return nil
	}
	var steps []step
	backup := workdir.Files{}
	for _, u := range all {
		name := u.domain.Name()
		e.show(name, u.changes)
		files, err := u.domain.Pull(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		live, err := u.domain.Items(files)
		if err != nil {
			return err
		}
		todo, conflicts := triage(u, live)
		if len(conflicts) > 0 {
			return fmt.Errorf("changed on the M-SERV since your pull: %s\nnothing written; pull --force starts over from the live state",
				strings.Join(conflicts, ", "))
		}
		if done := len(u.changes) - len(todo); done > 0 {
			fmt.Fprintf(e.Out, "%s: %d already on the M-SERV\n", name, done)
		}
		s := step{u, files, todo, nil}
		if len(todo) > 0 {
			if s.plan, err = u.domain.Prepare(todo, live, u.work); err != nil {
				return fmt.Errorf("%s: %w\nnothing written", name, err)
			}
			for _, n := range s.plan.Notes {
				fmt.Fprintf(e.Out, "%s: %s\n", name, n)
			}
		}
		steps = append(steps, s)
		maps.Copy(backup, files)
	}
	if !write {
		fmt.Fprintln(e.Out, "checked against the M-SERV; nothing written (push --yes writes)")
		return nil
	}
	// The backup holds every domain, so that it is the whole state the push started from.
	for _, d := range e.Domains {
		if slices.ContainsFunc(steps, func(s step) bool { return s.domain == d }) {
			continue
		}
		files, err := d.Pull(ctx)
		if err != nil {
			return fmt.Errorf("%s: backing up: %w\nnothing written", d.Name(), err)
		}
		maps.Copy(backup, files)
	}
	dir, err := e.Copy.Backup(backup)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "backup: %s\n", dir)
	for _, s := range steps {
		if s.plan == nil {
			if err := e.save(s.domain, s.files); err != nil {
				return err
			}
			continue
		}
		if err := recheck(ctx, s); err != nil {
			return fmt.Errorf("%s: %w\nnothing written to %s; push again to check and write it\nbackup of the state before this push: %s",
				s.domain.Name(), err, s.domain.Name(), dir)
		}
		if err := e.apply(ctx, s.domain, s.plan); err != nil {
			return fmt.Errorf("%s: %w\nbackup of the state before this push: %s", s.domain.Name(), err, dir)
		}
		fmt.Fprintf(e.Out, "%s: written and verified\n", s.domain.Name())
	}
	return nil
}

// step is one domain's part of a push.
type step struct {
	edits
	files workdir.Files // the live state when checked
	todo  []items.Change
	plan  *Plan // nil when the live state already has every edit
}

// recheck reads a domain's live state again just before it is written. The
// checks ran before the backup and the other domains' writes; this narrows
// the window for a change on the M-SERV to the write itself.
func recheck(ctx context.Context, s step) error {
	files, err := s.domain.Pull(ctx)
	if err != nil {
		return err
	}
	live, err := s.domain.Items(files)
	if err != nil {
		return err
	}
	todo, conflicts := triage(s.edits, live)
	if len(conflicts) > 0 {
		return fmt.Errorf("changed on the M-SERV during the push: %s", strings.Join(conflicts, ", "))
	}
	if len(todo) != len(s.todo) {
		return errors.New("some of the edits appeared on the M-SERV during the push")
	}
	for _, c := range s.plan.Expect {
		if _, taken := live[c.Key]; c.Added && taken {
			return fmt.Errorf("%s was created on the M-SERV during the push", c.Key)
		}
	}
	return nil
}

func (e *Engine) apply(ctx context.Context, d Domain, plan *Plan) error {
	if plan.Work != nil {
		if err := e.Copy.Work().Write(plan.Work, d.Owns); err != nil {
			return err
		}
	}
	if err := plan.Write(ctx); err != nil {
		return err
	}
	files, err := d.Pull(ctx)
	if err != nil {
		return fmt.Errorf("reading back: %w", err)
	}
	live, err := d.Items(files)
	if err != nil {
		return err
	}
	for _, c := range plan.Expect {
		got, ok := live[c.Key]
		switch {
		case applied(c, got, ok):
		case c.Removed:
			return fmt.Errorf("written, but %s is still there", c.Key)
		case !ok:
			return fmt.Errorf("written, but %s is missing", c.Key)
		default:
			return fmt.Errorf("written, but %s reads back differently:\n  %s", c.Key, strings.Join(items.Mismatch(c.After, got), "\n  "))
		}
	}
	if err := e.save(d, files); err != nil {
		return err
	}
	if plan.After != nil {
		return plan.After(ctx)
	}
	return nil
}

// triage splits a domain's edits into the changes still to write and the
// edited items that changed on the M-SERV since the pull. A change the live
// state already has is neither.
func triage(u edits, live items.Items) (todo []items.Change, conflicts []string) {
	for _, c := range u.changes {
		l, inLive := live[c.Key]
		if applied(c, l, inLive) {
			continue
		}
		b, inBase := u.base[c.Key]
		if inBase != inLive || !items.Equal(b, l) {
			conflicts = append(conflicts, c.Key)
			continue
		}
		todo = append(todo, c)
	}
	return todo, conflicts
}

// applied reports whether a live item (ok: it exists) reads as c leaves it.
// The M-SERV may add fields of its own; only the edited ones count.
func applied(c items.Change, live any, ok bool) bool {
	if c.Removed {
		return !ok
	}
	return ok && len(items.Mismatch(c.After, live)) == 0
}

// Restore puts a backup's files into the working copy, so that the next
// push writes them back. Domains the backup does not hold are left alone.
// Unpushed edits stop it unless force is set.
func (e *Engine) Restore(name string, force bool) error {
	t, err := e.Copy.BackupTree(name)
	if err != nil {
		return err
	}
	if !force {
		all, err := e.unpushed()
		if err != nil {
			return err
		}
		if len(all) > 0 {
			return fmt.Errorf("unpushed edits in %s (see ampiocli diff); push them, or restore --force to discard them", names(all))
		}
	}
	for _, d := range e.Domains {
		files, err := t.Read(d.Owns)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			continue
		}
		if err := e.Copy.Work().Write(files, d.Owns); err != nil {
			return err
		}
		fmt.Fprintf(e.Out, "%s: restored %d files; push to write them\n", d.Name(), len(files))
	}
	return nil
}

func names(all []edits) string {
	var out []string
	for _, u := range all {
		out = append(out, u.domain.Name())
	}
	return strings.Join(out, ", ")
}
