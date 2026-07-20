package erp

type PriceStockRequest struct {
	SKUList    []string
	PriceList  []string
	Warehouses []string
	UserExtID  string
	Date       string
}

type PriceStockItem struct {
	SKU              string
	Prices           map[string]float64
	StockByWarehouse map[string]float64
	Details          map[string]any
}

type PriceStockResult struct {
	Items []PriceStockItem
}
