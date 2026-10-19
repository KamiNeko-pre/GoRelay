//go:build !windows

package main

import "os"

// Linux deployment uses the existing constrained runner container. One child
// executes at a time; its container is a separate failure domain from SQL/Redis.
type containerChildIsolation struct{}

func (containerChildIsolation) Close()                 {}
func (containerChildIsolation) Err() error             { return nil }
func isolateChild(*os.Process) (childIsolation, error) { return containerChildIsolation{}, nil }
