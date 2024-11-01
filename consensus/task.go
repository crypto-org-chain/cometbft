package consensus

import "sync"

type TaskRunner struct {
	wg     sync.WaitGroup
	taskCh chan func()
}

func StartTaskRunner(buf int) *TaskRunner {
	tr := &TaskRunner{
		taskCh: make(chan func(), buf),
	}
	tr.wg.Add(1)
	go func() {
		defer tr.wg.Done()
		for f := range tr.taskCh {
			f()
		}
	}()
	return tr
}

func (tr *TaskRunner) StopAndWait() {
	close(tr.taskCh)
	tr.wg.Wait()
}

func (tr *TaskRunner) RunTask(f func()) {
	tr.taskCh <- f
}
