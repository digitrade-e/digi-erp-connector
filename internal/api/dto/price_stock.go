package dto

type PriceStockRequest struct {
	SKUList    []string `json:"skuList"`
	PriceList  []string `json:"priceList"`
	Warehouses []string `json:"warehouses"`
	UserExtID  string   `json:"userExtId"`
	Date       string   `json:"date,omitempty"`
}

type PriceStockItem struct {
	SKU              string             `json:"sku"`
	Prices           map[string]float64 `json:"prices,omitempty"`
	StockByWarehouse map[string]float64 `json:"stockByWarehouse,omitempty"`
	Details          map[string]any     `json:"details,omitempty"`
}

type PriceStockMeta struct {
	DurationMs int64 `json:"durationMs"`
}

type PriceStockResponse struct {
	Items []PriceStockItem `json:"items"`
	Meta  PriceStockMeta   `json:"meta"`
}
