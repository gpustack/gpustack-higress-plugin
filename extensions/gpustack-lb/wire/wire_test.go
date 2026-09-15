package wire

import (
	"encoding/json"
	"testing"
)

// gpustack serves several model names on one route from the **same cluster**,
// told apart only by targetId, and the rewrite target is resolved per targetId.
// So those candidates carry different ModelName values while sharing a cluster,
// and the scoring identity has to separate them.
func TestKeySeparatesCandidatesSharingACluster(t *testing.T) {
	a := Candidate{Cluster: "outbound|80||model-2-12.static", TargetID: "2"}
	b := Candidate{Cluster: "outbound|80||model-2-12.static", TargetID: "3"}

	if a.Key() == b.Key() {
		t.Fatalf("candidates on one cluster with different targets must not share a key: %q", a.Key())
	}
}

// With no targetId the key is the bare cluster name -- there is nothing to
// disambiguate, and keeping it unchanged means a route that never used targets
// keeps its existing session assignments.
func TestKeyWithoutTargetIsTheClusterName(t *testing.T) {
	c := Candidate{Cluster: "outbound|80||solo.static"}
	if got := c.Key(); got != "outbound|80||solo.static" {
		t.Errorf("Key() = %q, want the bare cluster name", got)
	}
}

// The separator has to be one that cannot occur in either half, or a cluster
// name containing it would collide with a different (cluster, targetId) pair --
// and the symptom would be two candidates silently sharing one score, which is
// the exact bug this key exists to prevent.
func TestKeyIsInjectiveAcrossTheSplit(t *testing.T) {
	// Adversarial: the separator's printable neighbours, and a cluster name
	// that ends where another's targetId begins.
	pairs := []Candidate{
		{Cluster: "a", TargetID: "b"},
		{Cluster: "a|b", TargetID: ""},
		{Cluster: "a", TargetID: "|b"},
		{Cluster: "a|", TargetID: "b"},
		{Cluster: "ab", TargetID: ""},
		{Cluster: "", TargetID: "ab"},
	}
	seen := map[string]Candidate{}
	for _, c := range pairs {
		k := c.Key()
		if prev, dup := seen[k]; dup {
			t.Errorf("collision: %+v and %+v both map to %q", prev, c, k)
		}
		seen[k] = c
	}
}

// The key rides through filter state as a JSON map key, so it has to survive a
// marshal/unmarshal round trip -- NUL is legal in a JSON string, but only
// because the encoder escapes it.
func TestKeySurvivesJSONRoundTrip(t *testing.T) {
	c := Candidate{Cluster: "outbound|80||model-2-12.static", TargetID: "2"}
	entry := RankEntry{Name: "x", Weight: 1, Scores: map[string]float64{c.Key(): 0.5}}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var back RankEntry
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if _, ok := back.Scores[c.Key()]; !ok {
		t.Errorf("key did not survive the round trip; got %q", data)
	}
}
