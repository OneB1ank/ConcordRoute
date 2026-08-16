package runtime

import (
	"errors"
	"net"
	"strconv"
	"sync"
)

const (
	DefaultMixedPortStart      = 17000
	DefaultMixedPortEnd        = 17999
	DefaultControllerPortStart = 18000
	DefaultControllerPortEnd   = 18999
)

type PortAllocator struct {
	mu              sync.Mutex
	mixedStart      int
	mixedEnd        int
	controllerStart int
	controllerEnd   int
	isFree          func(int) bool
}

func NewPortAllocator() *PortAllocator {
	return &PortAllocator{
		mixedStart:      DefaultMixedPortStart,
		mixedEnd:        DefaultMixedPortEnd,
		controllerStart: DefaultControllerPortStart,
		controllerEnd:   DefaultControllerPortEnd,
	}
}

func (a *PortAllocator) AllocatePair() (int, int, error) {
	if a == nil {
		a = NewPortAllocator()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	isFree := a.isFree
	if isFree == nil {
		isFree = portIsFree
	}
	mixed, err := firstFreePort(a.mixedStart, a.mixedEnd, isFree)
	if err != nil {
		return 0, 0, err
	}
	controller, err := firstFreePort(a.controllerStart, a.controllerEnd, isFree)
	if err != nil {
		return 0, 0, err
	}
	return mixed, controller, nil
}

func firstFreePort(start, end int, isFree func(int) bool) (int, error) {
	if start <= 0 || end < start {
		return 0, errors.New("port range is invalid")
	}
	for port := start; port <= end; port++ {
		if isFree == nil || isFree(port) {
			return port, nil
		}
	}
	return 0, errors.New("no free port in range")
}

func portIsFree(port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
