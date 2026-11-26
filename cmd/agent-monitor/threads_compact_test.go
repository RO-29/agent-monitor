package main

import (
	"encoding/json"
	"net/url"
	"reflect"
	"testing"
)

func TestShapeThreads(t *testing.T) {
	th := []ThreadV3{
		{Thread: Thread{ID: "a", Title: "A", Sessions: []string{"s1"}, LastAt: 100, LastOutcome: "long outcome", SegWeights: []int64{1}}, V4Fields: V4Fields{Display: "A!", Activity: []int{1}}},
		{Thread: Thread{ID: "b", Title: "B", Sessions: []string{"s2"}, LastAt: 300}},
		{Thread: Thread{ID: "c", Title: "C", Sessions: []string{"s3"}, LastAt: 200}},
	}
	res := map[string]any{"threads": th, "threadOf": map[string]string{"s1": "a", "s2": "b", "s3": "c"}, "stacks": []Stack{}}
	if got := shapeThreads(res, url.Values{}); !reflect.DeepEqual(got, res) {
		t.Fatal("no params must return the response unchanged")
	}
	got := shapeThreads(res, url.Values{"limit": {"2"}, "since": {"150"}})
	rows := got["threads"].([]ThreadV3)
	if len(rows) != 2 || rows[0].ID != "b" || rows[1].ID != "c" {
		t.Fatalf("since+limit rows %+v", rows)
	}
	if of := got["threadOf"].(map[string]string); len(of) != 2 || of["s1"] != "" {
		t.Fatalf("threadOf %v", of)
	}
	c := shapeThreads(res, url.Values{"compact": {"1"}})
	raw, _ := json.Marshal(c)
	var back struct {
		Threads []map[string]any
		Stacks  any
	}
	_ = json.Unmarshal(raw, &back)
	if len(back.Threads) != 3 || back.Stacks != nil {
		t.Fatalf("compact %s", raw)
	}
	for _, k := range []string{"lastOutcome", "segWeights", "activity", "tokens", "edges"} {
		if _, ok := back.Threads[0][k]; ok {
			t.Fatalf("compact row keeps %s", k)
		}
	}
	if back.Threads[0]["display"] != "A!" || back.Threads[0]["title"] != "A" {
		t.Fatalf("compact row %v", back.Threads[0])
	}
	if len(res["threads"].([]ThreadV3)) != 3 {
		t.Fatal("shaping changed the input")
	}
}
