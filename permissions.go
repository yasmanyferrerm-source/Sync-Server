package main

// ---------------------------------------------------------------------------
// Permissions — the one place that says what each role may do.
//
// The server enforces the data rules below (what a role receives and may
// change). The same list is sent to the app at sign-in ("perms") so its menus
// and screens follow the server instead of keeping their own copy.
// ---------------------------------------------------------------------------

// Permission keys. ".view" means read-only.
const (
	pReceiving     = "receiving"
	pReceivingView = "receiving.view"
	pShipping      = "shipping"
	pShippingView  = "shipping.view"
	pCounts        = "counts"
	pCountsView    = "counts.view"
	pItems         = "items"
	pItemCosts     = "items.costs"
	pOrders        = "orders"
	pOrdersView    = "orders.view"
	pInvoices      = "invoices"
	pExpenses      = "expenses"
	pReports       = "reports"
	pReportsStock  = "reports.stock"
	pUsers         = "users"
	pCompany       = "company"
	pCompanyDelete = "company.delete"
)

var rolePerms = map[string][]string{
	"admin": {pReceiving, pShipping, pCounts, pItems, pItemCosts, pOrders, pInvoices, pExpenses, pReports, pUsers, pCompany, pCompanyDelete},
	// Office: everything financial, view-only on warehouse work (plan default, easy to change here).
	"office": {pReceivingView, pShippingView, pCountsView, pItems, pItemCosts, pOrders, pInvoices, pExpenses, pReports},
	// Warehouse: floor work only; never receives costs, invoices or expenses.
	"warehouse": {pReceiving, pShipping, pCounts, pItems, pOrdersView, pReportsStock},
}

func permsFor(role string) []string {
	p := rolePerms[role]
	out := make([]string, len(p))
	copy(out, p)
	return out
}

func roleCan(role, perm string) bool {
	for _, p := range rolePerms[role] {
		if p == perm {
			return true
		}
	}
	return false
}

// Collections a role without the "invoices"/"expenses" permissions never sees
// or changes. Checked by filter.go on every read and every push.
func hiddenCollsFor(role string) map[string]bool {
	out := map[string]bool{}
	if !roleCan(role, pExpenses) {
		out["expenses"] = true
	}
	if !roleCan(role, pInvoices) {
		out["vendorInvoices"] = true
		out["vendorinv"] = true
		out["carrierInvoices"] = true // freight paid
	}
	return out
}

// Cost fields stripped from every item for a role without "items.costs".
var itemCostFields = []string{"cost", "avgCost", "lastCost", "stdCost", "landedCost", "margin", "unitCost"}
