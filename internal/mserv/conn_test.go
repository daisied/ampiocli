package mserv_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/daisied/ampiocli/internal/mserv"
	"github.com/daisied/ampiocli/internal/sim"
	"github.com/daisied/ampiocli/internal/workdir"
)

func conn(t *testing.T) *mserv.Conn {
	t.Helper()
	s, err := sim.Start(workdir.Files{}, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	c := mserv.New(s.Settings)
	t.Cleanup(c.Close)
	return c
}

func TestCallNoAnswer(t *testing.T) {
	c := conn(t)
	err := c.Call(context.Background(), "nobody/listens", nil, "nobody/answers", 100*time.Millisecond,
		func([]byte) (bool, error) { return true, nil })
	if !errors.Is(err, mserv.ErrNoAnswer) {
		t.Errorf("want ErrNoAnswer, got %v", err)
	}
}

func TestCallCancelled(t *testing.T) {
	c := conn(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := c.Call(ctx, "device_api/to/list", nil, "device_api/from/list", time.Minute,
		func([]byte) (bool, error) { return true, nil })
	if !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Errorf("want context.Canceled at once, got %v after %s", err, time.Since(start))
	}
}

func TestCallAnswer(t *testing.T) {
	c := conn(t)
	got, err := c.Get(context.Background(), "device_api/to/list", "", "device_api/from/list")
	if err != nil || string(got) != `{"devices":null}` {
		t.Errorf("Get: %s, %v", got, err)
	}
}
