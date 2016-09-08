package port_pool

import (
	"fmt"
	"sync"
)

type PortPool struct {
	start uint32
	size  uint32

	pools      [][]uint32
	poolMutex sync.Mutex

	states States
}

type PoolExhaustedError struct{}

func (e PoolExhaustedError) Error() string {
	return "port pool is exhausted"
}

type PortTakenError struct {
	Port uint32
}

func (e PortTakenError) Error() string {
	return fmt.Sprintf("port already acquired: %d", e.Port)
}

func New(start, size uint32, groups uint32, states States) (*PortPool, error) {
	if start+size > 65535 {
		return nil, fmt.Errorf("port_pool: New: invalid port range: startL %d, size: %d", start, size)
	}

	//call new() for each one
	pools := make ([][]uint32, groups)
	step := size/groups
	for i, _ := range pools {
		pools[i], _ = fill(start+step* uint32(i), step, states[i])
		i++
	}

	return &PortPool{
		start: start,
		size:  size,

		pools: pools,
	}, nil
}

func fill (start, size uint32, state State) ([]uint32, error) {
	if state.Offset >= size {
		state.Offset = 0
	}

	pool := make([]uint32, size)

	i := 0
	for port := start + state.Offset; port < start+size; port++ {
		pool[i] = port
		i += 1
	}
	for port := start; port < start+state.Offset; port++ {
		pool[i] = port
		i += 1
	}
	return pool, nil
}

func (p *PortPool) Acquire(index int) (uint32, error) {
	p.poolMutex.Lock()
	defer p.poolMutex.Unlock()
        i := uint32(index)
	if index > len(p.pools) {
		i = 0
	}
	var port uint32
	port, p.pools[i]= acquire(p.pools[i])
	if port == 0 {
		return 0, PoolExhaustedError{}
	}
	return port, nil
}

func acquire(pool []uint32) (uint32, []uint32) {
	if len(pool) == 0 {
		return 0, pool
	}

	port := pool[0]

	pool = pool[1:]

	return port, pool
}



func (p *PortPool) Remove(port uint32) error {
	idx := 0
	found := false

	p.poolMutex.Lock()
	defer p.poolMutex.Unlock()
//add one more loop, all the code can be contained inside
	for j, pool := range p.pools {
		for i, existingPort := range pool {
			if existingPort == port {
				idx = i
				found = true
				break
			}
		}
		p.pools[j] = append(p.pools[j][:idx], p.pools[j][idx+1:]...)


	}

	if !found {
		return PortTakenError{port}
	}

	return nil
}

func (p *PortPool) Release(port uint32) {
	if port < p.start || port >= p.start+p.size {
		return
	}

	p.poolMutex.Lock()
	defer p.poolMutex.Unlock()
	//add one more loop, all the code inlcude the append stays in
	for _, pool := range p.pools {
		for _, existingPort := range pool {
			if existingPort == port {
				return
			}
		}
	}
	// find the right pool to add
	for i:=0 ; i<len(p.pools); i++ {
		if port >= p.start + uint32(i) * p.size/uint32(len(p.pools)){
			p.pools[i] = append(p.pools[i], port)
			return
		}
	}


}

func (p *PortPool) RefreshState() States {
	//add a loop for all the pools
	var states States
	for i, pool := range p.pools {
		var state State
		if len(pool) == 0 {
			state.Offset = 0
		} else {
			state.Offset = pool[0] - p.start - uint32(i) * p.size/uint32(len(p.pools))
			if (state.Offset > p.size) {
				state.Offset = 0
			}
		}
		states = append(states,state)
	}
	return states
}
