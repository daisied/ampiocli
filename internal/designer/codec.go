// Package designer runs the module-logic codec of the M-SERV's own Designer
// web app: the code that turns a CAN module's rule bytes into readable rules
// and back.
//
// Designer is fetched from the M-SERV (and cached), never shipped with
// ampiocli, so the codec always matches the installation. Only the codec's
// code is taken out of it (see slice.go) and run in goja, an embedded
// JavaScript interpreter with no network or file access.
package designer

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"path"

	"github.com/dop251/goja"
)

//go:embed runtime.js
var runtimeJS string

// Codec is Designer's module-logic codec with the installation's modules registered.
type Codec struct {
	Version        string // the codec chunk's file name; it changes with every Designer release
	decode, encode goja.Callable
	vm             *goja.Runtime
}

// Open loads the codec from the Designer at designerURL and registers the
// CAN modules in devices (the device_api/from/list "devices" array).
func Open(designerURL string, devices json.RawMessage) (*Codec, error) {
	src, err := newSource(designerURL)
	if err != nil {
		return nil, err
	}
	file, err := src.codecChunk()
	if err != nil {
		return nil, err
	}
	p := newProgram(src)
	entry, err := p.chunk(file)
	if err != nil {
		return nil, err
	}
	names, err := entry.codecNames()
	if err != nil {
		return nil, err
	}
	code, err := p.build(entry, names)
	if err != nil {
		return nil, err
	}

	c := &Codec{Version: path.Base(file), vm: goja.New()}
	if err := c.start(code, devices); err != nil {
		return nil, fmt.Errorf("starting Designer %s codec: %w", c.Version, err)
	}
	return c, nil
}

func (c *Codec) start(js string, devices json.RawMessage) error {
	if _, err := c.vm.RunString(runtimeJS); err != nil {
		return err
	}
	fn, err := c.vm.RunString(js)
	if err != nil {
		return err
	}
	run, _ := goja.AssertFunction(fn)
	result, err := run(goja.Undefined())
	if err != nil {
		return err
	}
	// goja runs the program's awaits before the call returns.
	promise := result.Export().(*goja.Promise)
	switch promise.State() {
	case goja.PromiseStatePending:
		return errors.New("its start-up never finished (an import cycle?)")
	case goja.PromiseStateRejected:
		return fmt.Errorf("%v", promise.Result())
	}
	wrap, _ := goja.AssertFunction(c.vm.Get("codec"))
	api, err := wrap(goja.Undefined(), promise.Result(), c.vm.ToValue(string(devices)))
	if err != nil {
		return err
	}
	obj := api.ToObject(c.vm)
	c.decode, _ = goja.AssertFunction(obj.Get("decode"))
	c.encode, _ = goja.AssertFunction(obj.Get("encode"))
	return nil
}

// Decode turns a module's condition and linked-condition tables into rules.
// mac is Designer's "0_<hex>" form.
func (c *Codec) Decode(mac string, conditions, linked []byte) (json.RawMessage, error) {
	v, err := c.decode(goja.Undefined(), c.vm.ToValue(mac),
		c.vm.ToValue(c.vm.NewArrayBuffer(conditions)), c.vm.ToValue(c.vm.NewArrayBuffer(linked)))
	if err != nil {
		return nil, err
	}
	return json.RawMessage(v.String()), nil
}

// Encode turns rules into the condition and linked-condition tables Designer
// would write to the module.
func (c *Codec) Encode(mac string, rules json.RawMessage) (conditions, linked []byte, err error) {
	v, err := c.encode(goja.Undefined(), c.vm.ToValue(mac), c.vm.ToValue(string(rules)))
	if err != nil {
		return nil, nil, err
	}
	out, ok := v.Export().([]any)
	if !ok || len(out) != 2 {
		return nil, nil, errors.New("codec returned an unexpected value")
	}
	return out[0].(goja.ArrayBuffer).Bytes(), out[1].(goja.ArrayBuffer).Bytes(), nil
}
