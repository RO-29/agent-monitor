package main

import (
	"errors"
	"testing"
)

func TestCaptureWatchTargetsBatchAndFallback(t *testing.T) {
	prevB, prevOne := capturePanesFn, tmuxCaptureFn
	t.Cleanup(func() { capturePanesFn, tmuxCaptureFn = prevB, prevOne })
	single := 0
	tmuxCaptureFn = func(p string, _ int, _ bool) (string, error) {
		single++
		if p == "%dead" {
			return "", errors.New("gone")
		}
		return "one " + p, nil
	}
	wts := []promptWatchTarget{{"a", "%1"}, {"b", "%2"}}
	capturePanesFn = func(ids []string, _ int) (map[string]string, error) {
		return map[string]string{"%1": "x", "%2": "y"}, nil
	}
	if m := captureWatchTargets(wts); m["%1"] != "x" || m["%2"] != "y" || single != 0 {
		t.Fatalf("batch: %v, %d single captures", m, single)
	}
	// One dead pane fails the batch; the live panes still come back one by one.
	capturePanesFn = func([]string, int) (map[string]string, error) { return nil, errors.New("can't find pane") }
	m := captureWatchTargets(append(wts, promptWatchTarget{"c", "%dead"}))
	if m["%1"] != "one %1" || m["%2"] != "one %2" || len(m) != 2 {
		t.Fatalf("fallback: %v", m)
	}
	// The next tick captures the live panes in one call and leaves the dead one out of it.
	var asked []string
	capturePanesFn = func(ids []string, _ int) (map[string]string, error) {
		asked = ids
		return map[string]string{"%1": "x", "%2": "y"}, nil
	}
	captureWatchTargets(append(wts, promptWatchTarget{"c", "%dead"}))
	if len(asked) != 2 || asked[0] != "%1" || asked[1] != "%2" {
		t.Fatalf("batch after a dead pane: %v", asked)
	}
	t.Cleanup(func() { delete(badPanes, "%dead") })
}
