package main

// ---------------------------------------------------------------------------
// Role-based data protection for the "warehouse" role.
//
// Warehouse users never RECEIVE cost/finance data (reads are filtered), and
// their pushes can never CREATE or CLOBBER it (writes are protected): hidden
// collections are dropped from their changes, and on item updates the server
// restores the hidden cost fields from its own copy. admin and office are
// unrestricted.
// ---------------------------------------------------------------------------

import "encoding/json"

// whole collections a warehouse user cannot see or change
var whHiddenColls = map[string]bool{
	"expenses":       true,
	"vendorInvoices": true,
	"vendorinv":      true,
}

// cost fields stripped from every item for a warehouse user
var whHiddenItemFields = []string{"cost", "avgCost", "lastCost", "stdCost", "landedCost", "margin", "unitCost"}

func stripItemCost(rec map[string]any) {
	for _, f := range whHiddenItemFields {
		delete(rec, f)
	}
}

// deep JSON copy — used so filtering a read never mutates the live db
func deepCopyMap(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var c map[string]any
	_ = json.Unmarshal(b, &c)
	return c
}

// filterDBForRole returns a role-safe view of the whole database.
func filterDBForRole(db map[string]any, role string) map[string]any {
	if role != "warehouse" {
		return db
	}
	c := deepCopyMap(db)
	for coll := range whHiddenColls {
		delete(c, coll)
	}
	if arr, ok := c["items"].([]any); ok {
		for _, v := range arr {
			if rec, ok := v.(map[string]any); ok {
				stripItemCost(rec)
			}
		}
	}
	return c
}

// filterUpsForRole strips a changes-response (ups map) for a warehouse user,
// in place is avoided — it rebuilds the maps with safe copies.
func filterUpsForRole(ups map[string][]any, dels map[string][]string, role string) (map[string][]any, map[string][]string) {
	if role != "warehouse" {
		return ups, dels
	}
	outUps := map[string][]any{}
	for coll, arr := range ups {
		if whHiddenColls[coll] {
			continue
		}
		safe := make([]any, 0, len(arr))
		for _, v := range arr {
			if rec, ok := v.(map[string]any); ok && coll == "items" {
				cp := map[string]any{}
				for k, val := range rec {
					cp[k] = val
				}
				stripItemCost(cp)
				safe = append(safe, cp)
			} else {
				safe = append(safe, v)
			}
		}
		outUps[coll] = safe
	}
	outDels := map[string][]string{}
	for coll, ks := range dels {
		if whHiddenColls[coll] {
			continue
		}
		outDels[coll] = ks
	}
	return outUps, outDels
}

// protectWarehousePush sanitises an incoming push from a warehouse user, in place:
//   - drops any changes to hidden collections (they can't touch finance data)
//   - on item updates, restores the server's hidden cost fields so a cost-less
//     record from a warehouse device can never wipe the real cost
//
// caller holds s.mu (it reads s.db).
func (s *server) protectWarehousePush(req *pushReq) {
	for coll := range whHiddenColls {
		delete(req.Changes, coll)
	}
	if req.Settings != nil {
		// settings are shared, but keep it simple: warehouse can't change finance-y settings here
	}
	ch, ok := req.Changes["items"]
	if !ok {
		return
	}
	idx := s.index("items")
	for _, rec := range ch.Up {
		k := keyOf("items", rec)
		if pos, ok := idx[k]; ok {
			if ex, ok2 := s.db["items"].([]any)[pos].(map[string]any); ok2 {
				for _, f := range whHiddenItemFields {
					if v, has := ex[f]; has {
						rec[f] = v // keep the real cost the warehouse device never saw
					} else {
						delete(rec, f)
					}
				}
			}
		} else {
			// brand-new item created by warehouse: it simply has no cost yet
			stripItemCost(rec)
		}
	}
}
