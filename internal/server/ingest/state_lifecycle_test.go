package ingest

import "testing"

func TestPruneStatesRetainsReferencedAndNewlyTouchedState(t *testing.T) {
	service := &Service{states: make(map[string]*nodeState)}
	state := service.acquireState("node-a")
	service.releaseState(state)
	candidates := service.stateCandidates()

	reacquired := service.acquireState("node-a")
	service.releaseState(reacquired)
	service.pruneStates(map[string]bool{}, candidates)
	service.mu.Lock()
	retained := service.states["node-a"]
	service.mu.Unlock()
	if retained != state {
		t.Fatal("stale prune snapshot replaced a state touched after the scan began")
	}

	candidates = service.stateCandidates()
	service.pruneStates(map[string]bool{}, candidates)
	service.mu.Lock()
	_, exists := service.states["node-a"]
	service.mu.Unlock()
	if exists {
		t.Fatal("prune retained an unreferenced deleted node state")
	}
}
