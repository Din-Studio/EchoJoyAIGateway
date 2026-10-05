package affinity

import "time"

// Target is the exact Credential identity remembered as a soft preference.
type Target struct {
	GroupID            uint
	CredentialID       uint
	IdentityGeneration uint64
}

func (target Target) Valid() bool {
	return target.GroupID != 0 && target.CredentialID != 0 && target.IdentityGeneration != 0
}

// Policy is the frozen affinity configuration of one published snapshot.
type Policy struct {
	Revision uint64
	Capacity int
	TTL      time.Duration
}

// Valid reports whether the policy enables affinity at all.
func (policy Policy) Valid() bool {
	return policy.Revision != 0 && policy.Capacity > 0 && policy.TTL > 0
}

// Observation is a store lookup used for conditional success updates.
type Observation struct {
	Target Target
	// token is the stored value a shared store compares before overwriting.
	token string
}

func (observation Observation) Found() bool {
	return observation.token != ""
}

// SharedObservation records what a shared store saw. An empty token means no
// live mapping was observed.
func SharedObservation(target Target, token string) Observation {
	return Observation{Target: target, token: token}
}

// Token returns the stored value observed by a shared store.
func (observation Observation) Token() string {
	return observation.token
}
