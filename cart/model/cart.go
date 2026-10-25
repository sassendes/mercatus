package model

import "time"

type CartItem struct {
	ID          int64          `json:"id"`
	CartID      int64          `json:"cart_id"`
	VariantID   int64          `json:"variant_id"`
	ProductID   int64          `json:"product_id"`
	SellerID    int64          `json:"seller_id"`
	StoreName   string         `json:"store_name"`
	Title       string         `json:"title"`
	Slug        string         `json:"slug"`
	ImageURL    string         `json:"image_url"`
	Category    string         `json:"category_slug"`
	SKU         string         `json:"sku"`
	Attributes  map[string]any `json:"attributes"`
	Quantity    int            `json:"quantity"`
	UnitPrice   int64          `json:"unit_price"`
	Currency    string         `json:"currency"`
	Available   int            `json:"available"`
	Purchasable bool           `json:"purchasable"`
	CreatedAt   time.Time      `json:"created_at"`
}

type Cart struct {
	ID        int64       `json:"id"`
	UserID    int64       `json:"user_id"`
	Items     []*CartItem `json:"items"`
	ItemCount int         `json:"item_count"`
	Subtotal  int64       `json:"subtotal"`
	Currency  string      `json:"currency"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

type VariantInfo struct {
	VariantID    int64
	SellerID     int64
	SellerUserID int64
	Price        int64
	Currency     string
	Available    int
}
