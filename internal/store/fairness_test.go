package store

import (
	"reflect"
	"testing"
)

func TestAllocateClaims(t *testing.T) {
	for _, tt := range []struct {
		name      string
		capacity  []int
		limit     int
		wantTake  []int
		wantOrder []string
	}{
		{"partial round", []int{10, 10, 10}, 8, []int{3, 3, 2}, []string{"c", "a", "b"}},
		{"one slot", []int{10, 10, 10}, 1, []int{1, 0, 0}, []string{"a"}},
		{"spare capacity", []int{1, 10, 2}, 10, []int{1, 7, 2}, []string{"a", "c", "b"}},
		{"single endpoint", []int{100}, 10, []int{10}, []string{"a"}},
		{"no work", []int{0, 0}, 10, []int{0, 0}, nil},
		{"less work than slots", []int{2, 1}, 10, []int{2, 1}, []string{"b", "a"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			endpoints := make([]endpointAllocation, len(tt.capacity))
			for i, capacity := range tt.capacity {
				endpoints[i] = endpointAllocation{id: string(rune('a' + i)), capacity: capacity}
			}
			got := allocateClaims(endpoints, tt.limit)
			takes := make([]int, len(tt.capacity))
			var order []string
			for _, ep := range got {
				takes[int(ep.id[0]-'a')] = ep.take
				if ep.take > 0 {
					order = append(order, ep.id)
				}
			}
			if !reflect.DeepEqual(takes, tt.wantTake) || !reflect.DeepEqual(order, tt.wantOrder) {
				t.Fatalf("takes=%v order=%v", takes, order)
			}
		})
	}
}

func TestAllocateClaimsAccountsForOccupiedSlots(t *testing.T) {
	endpoints := []endpointAllocation{
		{id: "busy", active: 8, capacity: 2},
		{id: "idle", active: 0, capacity: 10},
	}
	got := allocateClaims(endpoints, 10)
	for _, ep := range got {
		want := 9
		if ep.id == "busy" {
			want = 1
		}
		if ep.take != want {
			t.Fatalf("allocation=%+v", got)
		}
	}
}
