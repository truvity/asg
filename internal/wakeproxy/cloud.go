// Package wakeproxy is the engine of cmd/wake-proxy: a TCP pass-through proxy
// that starts a stopped EC2 instance when a client connects, holds the
// client's connection while it wakes, pipes the bytes (it never terminates
// TLS) and stops the instance after a period without connections.
package wakeproxy

import (
	"context"
	"errors"
	"fmt"
)

// Instance states the proxy acts on.
const (
	StatePending  = "pending"
	StateRunning  = "running"
	StateStopping = "stopping"
	StateStopped  = "stopped"
)

// Instance is what the proxy needs to know about a target.
type Instance struct {
	ID        string
	State     string
	PrivateIP string
	Tags      map[string]string
}

// Cloud is the part of EC2 the proxy uses. cmd/wake-proxy implements it with
// the AWS SDK; the tests implement it with a fake.
type Cloud interface {
	// Find returns the instances that carry the tag and are not shutting
	// down or terminated.
	Find(ctx context.Context, tagKey, tagValue string) ([]Instance, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	// Tag sets one tag.
	Tag(ctx context.Context, id, key, value string) error
}

// Errors of the target lookup and the wake.
var (
	ErrNoTarget    = errors.New("no instance matches the selector")
	ErrAmbiguous   = errors.New("more than one instance matches the selector")
	ErrWakeTimeout = errors.New("the target did not accept connections before the wake timeout")
)

// lookup returns the one instance the selector matches, or an error.
func lookup(ctx context.Context, c Cloud, key, value string) (Instance, error) {
	found, err := c.Find(ctx, key, value)
	if err != nil {
		return Instance{}, fmt.Errorf("describe instances: %w", err)
	}

	switch len(found) {
	case 0:
		return Instance{}, fmt.Errorf("%w (tag %s=%s)", ErrNoTarget, key, value)
	case 1:
		return found[0], nil
	default:
		return Instance{}, fmt.Errorf("%w (tag %s=%s, %d instances)", ErrAmbiguous, key, value, len(found))
	}
}
