package app

import (
	"sync"
	"time"
)

// startTicker runs fn on the given interval and returns a stop function that
// signals the goroutine to exit and waits for it to finish.
func startTicker(interval time.Duration, fn func()) func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				fn()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}
