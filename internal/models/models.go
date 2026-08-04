package models

import "errors"

var ErrOutOfStock = errors.New("out of stock")

type Category struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	ImageURL string `json:"imageUrl"`
}

type Product struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	UnitPrice  int64    `json:"unitPrice"`
	Stock      int      `json:"stock"`
	CategoryID int64    `json:"categoryId"`
	Images     []string `json:"images"`
}

type OrderItem struct {
	ProductID int64  `json:"productId"`
	Qty       int    `json:"quantity"`
	UnitPrice int64  `json:"unitPrice"`
	ImageURL  string `json:"imageUrl"`
}

type Order struct {
	ID         int64       `json:"id"`
	CustomerID int64       `json:"customerId"`
	Total      int64       `json:"total"`
	Status     string      `json:"status"`
	CreatedAt  string      `json:"createdAt"`
	Items      []OrderItem `json:"items"`
}

type Customer struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Surname  string `json:"surname"`
	Password string `json:"password"`
	Email    string `json:"email"`
}

type LoginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type CartRequest struct {
	CustomerID int64 `json:"customerId"`
	ProductID  int64 `json:"product_id"`
	Quantity   int   `json:"quantity"`
}

type Line struct {
	ProductID int64 `json:"productId"`
	Qty       int   `json:"quantity"`
}

type CheckoutReq struct {
	CustomerID int64  `json:"customerId"`
	Lines      []Line `json:"lines"`
}

type CartItem struct {
	ProductID   int64  `json:"productId"`
	ProductName string `json:"productName"`
	Quantity    int    `json:"quantity"`
	UnitPrice   int64  `json:"unitPrice"`
	ImageURL    string `json:"imageUrl"`
}

type Cart struct {
	Items []CartItem `json:"items"`
	Total int64      `json:"total"`
}

type RecipeRequest struct {
	CustomerID int64  `json:"customerId"`
	Recipe     string `json:"recipe"`
}

type RecipeItem struct {
	Isim string `json:"isim"`
	Adet int    `json:"adet"`
}
type CancelRequest struct {
	Action  string `json:"action"`
	OrderID int64  `json:"orderId"`
	Summary string `json:"summary"`
}

type SupportRequest struct {
	Message string `json:"message"`
}

type SupportProposal struct {
	Action               string `json:"action"`
	OrderID              int64  `json:"orderId"`
	Summary              string `json:"summary"`
	RequiresConfirmation bool   `json:"requiresConfirmation"`
}

type SupportResponse struct {
	Answer   string           `json:"answer,omitempty"`
	Proposal *SupportProposal `json:"proposal,omitempty"`
}
