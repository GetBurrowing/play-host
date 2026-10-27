package capture

import (
	"errors"
	"runtime"
	"sync"
)

// SetThreadExecutionState flag values.
const (
	ESSystemRequired  uint32 = 0x00000001
	ESDisplayRequired uint32 = 0x00000002
	ESContinuous      uint32 = 0x80000000
)

// StateSetter is SetThreadExecutionState: it applies flags to the calling
// thread and reports success. The windows build supplies the real one; tests
// and other platforms supply a fake.
type StateSetter func(flags uint32) bool

type threadKeepAwake struct {
	set  StateSetter
	mu   sync.Mutex
	stop chan struct{}
	done chan struct{}
}

// NewKeepAwakeWith builds a KeepAwake on set. The state is per thread, so
// Start applies ES_CONTINUOUS|ES_DISPLAY_REQUIRED|ES_SYSTEM_REQUIRED on a
// dedicated locked OS thread that lives until Stop, which clears it with
// ES_CONTINUOUS. It never touches powercfg.
func NewKeepAwakeWith(set StateSetter) KeepAwake { return &threadKeepAwake{set: set} }

func (k *threadKeepAwake) Start() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stop != nil {
		return nil
	}
	stop, done, ready := make(chan struct{}), make(chan struct{}), make(chan bool, 1)
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		ok := k.set(ESContinuous | ESDisplayRequired | ESSystemRequired)
		ready <- ok
		if !ok {
			return
		}
		<-stop
		k.set(ESContinuous)
	}()
	if !<-ready {
		<-done
		return errors.New("keep-awake: SetThreadExecutionState failed")
	}
	k.stop, k.done = stop, done
	return nil
}

func (k *threadKeepAwake) Stop() {
	k.mu.Lock()
	stop, done := k.stop, k.done
	k.stop, k.done = nil, nil
	k.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
}
