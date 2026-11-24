package server

// Startup recovery must not delay HTTP admission while a slow worker
// drains a queue larger than its channel capacity. Each queue has its own
// tracked producer so a blocked archive worker cannot block import recovery.
func (a *App) restorePendingQueue(queue chan<- string, ids []string) {
	if len(ids) == 0 {
		return
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		for _, id := range ids {
			if a.ctx.Err() != nil {
				return
			}
			select {
			case <-a.ctx.Done():
				return
			case queue <- id:
			}
		}
	}()
}
