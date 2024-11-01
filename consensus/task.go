package consensus

// StartTaskRunner spawn a single goroutine to run tasks in FIFO order.
func StartTaskRunner(buf int) func(func()) {
	taskCh := make(chan func(), buf)
	go func() {
		for f := range taskCh {
			f()
		}
	}()
	return func(f func()) {
		taskCh <- f
	}
}
