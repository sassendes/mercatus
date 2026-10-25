package model

import "time"

type Order struct {
	ID         int64      `json:"id"`
	BuyerID    int64      `json:"buyer_id"`
	Status     string     `json:"status"`
	TotalCents int64      `json:"total_cents"`
	Currency   string     `json:"currency"`
	ItemCount  int        `json:"item_count"`
	Preview    []Preview  `json:"preview,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	SubOrders  []SubOrder `json:"sub_orders,omitempty"`
}

type Preview struct {
	Title    string `json:"title"`
	ImageURL string `json:"image_url"`
	Slug     string `json:"slug"`
	Category string `json:"category_slug"`
}

type SubOrder struct {
	ID              int64       `json:"id"`
	OrderID         int64       `json:"order_id"`
	SellerID        int64       `json:"seller_id"`
	StoreName       string      `json:"store_name"`
	Status          string      `json:"status"`
	SubtotalCents   int64       `json:"subtotal_cents"`
	CommissionCents int64       `json:"commission_cents"`
	Currency        string      `json:"currency"`
	CreatedAt       time.Time   `json:"created_at,omitzero"`
	BuyerEmail      string      `json:"buyer_email,omitempty"`
	Summary         string      `json:"summary,omitempty"`
	Items           []OrderItem `json:"items,omitempty"`
}

type OrderItem struct {
	ID             int64  `json:"id"`
	SubOrderID     int64  `json:"sub_order_id"`
	VariantID      int64  `json:"variant_id"`
	ProductID      int64  `json:"product_id"`
	Title          string `json:"title"`
	Slug           string `json:"slug"`
	ImageURL       string `json:"image_url"`
	Category       string `json:"category_slug"`
	Quantity       int    `json:"quantity"`
	UnitPriceCents int64  `json:"unit_price_cents"`
	Currency       string `json:"currency"`
}

type PricedLine struct {
	VariantID      int64
	ProductID      int64
	SellerID       int64
	SellerUserID   int64
	Title          string
	UnitPriceCents int64
	Currency       string
	CommissionBps  int
	Purchasable    bool
}

type SellerSplit struct {
	SellerID        int64
	SubtotalCents   int64
	CommissionCents int64
}
