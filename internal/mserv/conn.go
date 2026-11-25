package mserv

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// Conn is an MQTT session with the M-SERV. Everything Designer does to the
// configuration is a publish on one topic and a reply on another; Conn turns
// that into a call. It connects on first use, and calls run one at a time.
type Conn struct {
	settings Settings
	mu       sync.Mutex
	client   mqtt.Client // nil until connected
}

func New(s Settings) *Conn { return &Conn{settings: s} }

// ErrNoAnswer is returned by Call when the reply topic stays silent.
var ErrNoAnswer = errors.New("no answer")

// connect runs with c.mu held.
func (c *Conn) connect(ctx context.Context) error {
	if c.client != nil {
		return nil
	}
	s := c.settings
	if err := Need(s.MQTT, s.Password); err != nil {
		return err
	}
	opts := mqtt.NewClientOptions().
		AddBroker(s.MQTT).
		SetClientID(fmt.Sprintf("ampiocli-%d", time.Now().UnixNano())).
		SetUsername(s.User).
		SetPassword(s.Password).
		SetConnectTimeout(5 * time.Second).
		SetAutoReconnect(false)
	client := mqtt.NewClient(opts)
	if err := await(ctx, client.Connect()); err != nil {
		return fmt.Errorf("mqtt %s: %w", s.MQTT, err)
	}
	c.client = client
	return nil
}

func (c *Conn) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		c.client.Disconnect(250)
		c.client = nil
	}
}

// Get publishes payload on topic and returns the first reply on reply.
func (c *Conn) Get(ctx context.Context, topic, payload, reply string) ([]byte, error) {
	var out []byte
	err := c.Call(ctx, topic, []byte(payload), reply, 15*time.Second, func(p []byte) (bool, error) {
		out = p
		return true, nil
	})
	return out, err
}

// Publish sends a message that has no reply.
func (c *Conn) Publish(ctx context.Context, topic, payload string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.connect(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return await(ctx, c.client.Publish(topic, 0, false, payload))
}

// Call subscribes to reply, publishes payload on topic, and hands every new
// message on reply to until, returning when until says done (or fails).
// Retained messages are skipped: they are old answers, not ours.
func (c *Conn) Call(ctx context.Context, topic string, payload []byte, reply string, timeout time.Duration, until func([]byte) (bool, error)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.connect(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	msgs := make(chan []byte)
	handler := func(_ mqtt.Client, m mqtt.Message) {
		if m.Retained() {
			return
		}
		select {
		case msgs <- m.Payload():
		case <-ctx.Done():
		}
	}
	if err := await(ctx, c.client.Subscribe(reply, 0, handler)); err != nil {
		return err
	}
	defer c.client.Unsubscribe(reply)
	// A push interrupted before this point sends nothing more.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := await(ctx, c.client.Publish(topic, 0, false, payload)); err != nil {
		return err
	}
	for {
		select {
		case p := <-msgs:
			if done, err := until(p); err != nil || done {
				return err
			}
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("%w on %s after %s", ErrNoAnswer, reply, timeout)
			}
			return ctx.Err()
		}
	}
}

// await waits for an MQTT operation, at most 10 s, and no longer than ctx.
func await(ctx context.Context, t mqtt.Token) error {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-t.Done():
		return t.Error()
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("mqtt: timed out")
	}
}
