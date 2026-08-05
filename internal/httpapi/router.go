package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/google/generative-ai-go/genai"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rasim/aurora/internal/models"
	"github.com/rasim/aurora/internal/store"
	"google.golang.org/api/option"
)

func NewRouter(s *store.Store) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })


	mux.HandleFunc("GET /products", func(w http.ResponseWriter, r *http.Request) {
		searchQuery := r.URL.Query().Get("search")
		categoryIDStr := r.URL.Query().Get("category_id")
		var ps []models.Product
		var err error

		if searchQuery != "" {
			ps, err = s.SearchProducts(r.Context(), searchQuery)
		} else if categoryIDStr != "" {
			categoryID, parseErr := strconv.ParseInt(categoryIDStr, 10, 64)
			if parseErr != nil {
				writeJSON(w, 400, map[string]string{"error": "invalid_category_id"})
				return
			}
			ps, err = s.ProductsByCategory(r.Context(), categoryID)
		} else {
			ps, err = s.Products(r.Context())
		}

		if err != nil {
			slog.Error("products", "err", err)
			writeJSON(w, 500, map[string]string{"error": "internal"})
			return
		}

		if ps == nil {
			ps = []models.Product{}
		}

		writeJSON(w, 200, ps)
	})

	mux.HandleFunc("GET /categories", func(w http.ResponseWriter, r *http.Request) {
		cats, err := s.Categories(r.Context())
		if err != nil {
			slog.Error("categories", "err", err)
			writeJSON(w, 500, map[string]string{"error": "internal"})
			return
		}
		writeJSON(w, 200, cats)
	})

	mux.HandleFunc("POST /checkout", func(w http.ResponseWriter, r *http.Request) {
		var req models.CheckoutReq
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, 422, map[string]string{"error": "invalid_request"})
			return
		}
		if req.CustomerID == 0 {
			writeJSON(w, 422, map[string]string{"error": "invalid_request"})
			return
		}
		cartToken := r.Header.Get("X-Cart-Token")
		if cartToken == "" {
			writeJSON(w, 422, map[string]string{"error": "invalid_request"})
			return
		}
		idem := r.Header.Get("Idempotency-Key")

		orderID, err := s.Checkout(r.Context(), req.CustomerID, cartToken, idem)
		switch {
		case err == nil:
			writeJSON(w, 200, map[string]int64{"orderId": orderID})
		case errors.Is(err, models.ErrOutOfStock):
			writeJSON(w, 409, map[string]string{"error": "out_of_stock"})
		case errors.Is(err, models.ErrDealSoldOut):
			writeJSON(w, 409, map[string]string{"error": "deal_sold_out"})
		case errors.Is(err, models.ErrPurchaseLimit):
			writeJSON(w, 409, map[string]string{"error": "purchase_limit"})
		default:
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" {
				writeJSON(w, 404, map[string]string{"error": "not_found"})
				return
			}
			if idem != "" && errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "orders_idem_key" {
				if id, e := s.OrderIDByKey(r.Context(), idem); e == nil {
					writeJSON(w, 200, map[string]int64{"orderId": id})
					return
				}
			}
			writeJSON(w, 500, map[string]string{"error": "internal"})
		}
	})

	mux.HandleFunc("GET /products/{id}", func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid_id"})
			return
		}

		product, err := s.Product(r.Context(), id)

		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, 404, map[string]string{"error": "not_found"})
				return
			}
			slog.Error("get product", "err", err)
			writeJSON(w, 500, map[string]string{"error": "internal"})
			return
		}
		writeJSON(w, 200, product)
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		var req models.LoginReq
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "gecersiz_istek_formati"})
			return
		}
		if req.Email == "" || req.Password == "" {
			writeJSON(w, 400, map[string]string{"error": "email_ve_sifre_zorunlu"})
			return
		}
		user, err := s.CheckUserLogin(r.Context(), req.Email, req.Password)
		if err != nil {
			slog.Error("login hatasi", "err", err)
			writeJSON(w, 401, map[string]string{"error": "hatali_eposta_veya_sifre"})
			return
		}
		writeJSON(w, 200, map[string]any{
			"message": "giris_basarili",
			"userId":  user.ID,
			"name":    user.Name,
		})
	})

	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		var req models.Customer
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "gecersiz_istek_formati"})
			return
		}
		if req.Name == "" || req.Surname == "" || req.Email == "" || req.Password == "" {
			writeJSON(w, 400, map[string]string{"error": "tum_alanlar_zorunludur"})
			return
		}
		id, err := s.RegisterUser(r.Context(), req.Name, req.Surname, req.Email, req.Password)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				writeJSON(w, 409, map[string]string{"error": "eposta_zaten_kayitli"})
				return
			}
			slog.Error("register hatasi", "err", err)
			writeJSON(w, 500, map[string]string{"error": "internal"})
			return
		}
		writeJSON(w, 201, map[string]any{
			"message": "kayit_basarili",
			"userId":  id,
		})
	})

	mux.HandleFunc("POST /cart", func(w http.ResponseWriter, r *http.Request) {
		var req models.CartRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "geçersiz_veri"})
			return
		}

		if req.CustomerID == 0 {
			writeJSON(w, 401, map[string]string{"error": "kullanici_kimligi_gerekli"})
			return
		}

		err := s.AddToCart(r.Context(), req.CustomerID, req.ProductID, req.Quantity)
		if err != nil {
			if err.Error() == "yetersiz stok veya ürün bulunamadı" {
				writeJSON(w, 409, map[string]string{"error": "yetersiz_stok"})
				return
			}
			slog.Error("sepete ekleme hatası", "err", err)
			writeJSON(w, 500, map[string]string{"error": "sunucu_hatasi"})
			return
		}
		writeJSON(w, 200, map[string]string{"message": "sepete_eklendi"})
	})

	mux.HandleFunc("GET /cart/{customerId}", func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("customerId")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id == 0 {
			writeJSON(w, 400, map[string]string{"error": "gecersiz_musteri_id"})
			return
		}

		cart, err := s.GetCart(r.Context(), id)
		if err != nil {
			slog.Error("get cart", "err", err)
			writeJSON(w, 500, map[string]string{"error": "sunucu_hatasi"})
			return
		}

		writeJSON(w, 200, cart)
	})

	mux.HandleFunc("PUT /cart", func(w http.ResponseWriter, r *http.Request) {
		var req models.CartRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "gecersiz_veri"})
			return
		}

		if req.CustomerID == 0 {
			writeJSON(w, 401, map[string]string{"error": "kullanici_kimligi_gerekli"})
			return
		}

		err := s.UpdateCartItemQuantity(r.Context(), req.CustomerID, req.ProductID, req.Quantity)
		if err != nil {
			if err.Error() == "yetersiz stok" {
				writeJSON(w, 409, map[string]string{"error": "yetersiz_stok"})
				return
			} else if err.Error() == "ürün sepette bulunamadı" {
				writeJSON(w, 404, map[string]string{"error": "urun_bulunamadi"})
				return
			}
			slog.Error("sepet guncelleme hatasi", "err", err)
			writeJSON(w, 500, map[string]string{"error": "sunucu_hatasi"})
			return
		}
		writeJSON(w, 200, map[string]string{"message": "sepet_guncellendi"})
	})

	mux.HandleFunc("DELETE /cart/{customerId}/{productId}", func(w http.ResponseWriter, r *http.Request) {
		customerIDStr := r.PathValue("customerId")
		productIDStr := r.PathValue("productId")

		customerID, err := strconv.ParseInt(customerIDStr, 10, 64)
		if err != nil || customerID == 0 {
			writeJSON(w, 400, map[string]string{"error": "gecersiz_musteri_id"})
			return
		}

		productID, err := strconv.ParseInt(productIDStr, 10, 64)
		if err != nil || productID == 0 {
			writeJSON(w, 400, map[string]string{"error": "gecersiz_urun_id"})
			return
		}

		err = s.RemoveFromCart(r.Context(), customerID, productID)
		if err != nil {
			if err.Error() == "ürün sepette bulunamadı" {
				writeJSON(w, 404, map[string]string{"error": "urun_bulunamadi"})
				return
			}
			slog.Error("sepetten silme hatasi", "err", err)
			writeJSON(w, 500, map[string]string{"error": "sunucu_hatasi"})
			return
		}
		writeJSON(w, 200, map[string]string{"message": "urun_sepetten_silindi"})
	})

	mux.HandleFunc("DELETE /cart/{customerId}", func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("customerId")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id == 0 {
			writeJSON(w, 400, map[string]string{"error": "gecersiz_musteri_id"})
			return
		}

		err = s.ClearCart(r.Context(), id)
		if err != nil {
			slog.Error("sepet temizleme hatasi", "err", err)
			writeJSON(w, 500, map[string]string{"error": "sunucu_hatasi"})
			return
		}

		writeJSON(w, 200, map[string]string{"message": "sepet_temizlendi"})
	})

	mux.HandleFunc("GET /orders/{customerId}", func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("customerId")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id == 0 {
			writeJSON(w, 400, map[string]string{"error": "gecersiz_musteri_id"})
			return
		}

		orders, err := s.OrdersByCustomer(r.Context(), id)
		if err != nil {
			slog.Error("get orders", "err", err)
			writeJSON(w, 500, map[string]string{"error": "sunucu_hatasi"})
			return
		}

		writeJSON(w, 200, orders)
	})
	mux.HandleFunc("POST /recipe-to-cart", func(w http.ResponseWriter, r *http.Request) {
		var req models.RecipeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "gecersiz_istek"})
			return
		}

		if req.CustomerID == 0 || req.Recipe == "" {
			writeJSON(w, 400, map[string]string{"error": "kullanici_kimligi_ve_tarif_zorunlu"})
			return
		}

		apiKey := os.Getenv("GEMINI_API_KEY")
		if apiKey == "" {
			slog.Error("GEMINI_API_KEY bulunamadi")
			writeJSON(w, 500, map[string]string{"error": "sunucu_yapilandirma_hatasi"})
			return
		}

		client, err := genai.NewClient(r.Context(), option.WithAPIKey(apiKey))
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "yapay_zeka_baglanti_hatasi"})
			return
		}
		defer client.Close()

		model := client.GenerativeModel("gemini-flash-latest")

		model.SystemInstruction = &genai.Content{
			Parts: []genai.Part{
				genai.Text(`You're an e-commerce data extraction assistant. If there is a recipe in the text entered by the user, only remove the products/ingredients and quantities. If there is a dish name, extract the ingredients and customs of the recipe for this dish. Never explain. If the unit of measurement of the material is not a piece, don't write according to that unit of measureme	nt. The quantity part should only be integer. The output must ONLY be an array in the following JSON format: [{"isim": "banana", "adet": 3}]`),
			},
		}

		model.ResponseMIMEType = "application/json"

		resp, err := model.GenerateContent(r.Context(), genai.Text(req.Recipe))
		if err != nil {
			slog.Error("gemini uretim hatasi", "err", err)
			writeJSON(w, 500, map[string]string{"error": "tarif_islenemedi"})
			return
		}

		var rawJSON string
		if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
			if txt, ok := resp.Candidates[0].Content.Parts[0].(genai.Text); ok {
				rawJSON = string(txt)
			}
		}

		rawJSON = strings.TrimSpace(rawJSON)
		if strings.HasPrefix(rawJSON, "```json") {
			rawJSON = strings.TrimPrefix(rawJSON, "```json")
			rawJSON = strings.TrimSuffix(rawJSON, "```")
			rawJSON = strings.TrimSpace(rawJSON)
		} else if strings.HasPrefix(rawJSON, "```") {
			rawJSON = strings.TrimPrefix(rawJSON, "```")
			rawJSON = strings.TrimSuffix(rawJSON, "```")
			rawJSON = strings.TrimSpace(rawJSON)
		}

		var items []models.RecipeItem
		if err := json.Unmarshal([]byte(rawJSON), &items); err != nil {
			slog.Error("gemini json parse hatasi", "err", err, "raw", rawJSON)
			writeJSON(w, 500, map[string]string{"error": "anlasilmayan_tarif_formati"})
			return
		}

		addedItems := []string{}
		notFoundItems := []string{}

		for _, item := range items {
			if item.Adet <= 0 {
				item.Adet = 1
			}

			products, err := s.SearchProducts(r.Context(), item.Isim)
			if err != nil || len(products) == 0 {
				notFoundItems = append(notFoundItems, item.Isim)
				continue
			}

			matchedProduct := products[0]

			err = s.AddToCart(r.Context(), req.CustomerID, matchedProduct.ID, item.Adet)
			if err != nil {
				notFoundItems = append(notFoundItems, fmt.Sprintf("%s (Stok Yetersiz)", matchedProduct.Name))
				continue
			}
			addedItems = append(addedItems, fmt.Sprintf("%s (%d Adet)", matchedProduct.Name, item.Adet))
		}

		writeJSON(w, 200, map[string]any{
			"message":          "Tarif islendi",
			"sepeteEklenenler": addedItems,
			"bulunamayanlar":   notFoundItems,
		})
	})
	mux.HandleFunc("POST /orders/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("id")
		orderID, _ := strconv.ParseInt(idStr, 10, 64)

		var req models.CancelRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad_request"})
			return
		}

		if req.Action != "cancel" || req.OrderID != orderID || req.CustomerID == 0 {
			writeJSON(w, 422, map[string]string{"error": "invalid_request"})
			return
		}

		err := s.CancelOrder(r.Context(), orderID, req.CustomerID)
		if err != nil {
			if err.Error() == "not_cancellable" {
				writeJSON(w, 409, map[string]string{"error": "not_cancellable"})
			} else if err.Error() == "not_found" {
				writeJSON(w, 404, map[string]string{"error": "not_found"})
			} else {
				writeJSON(w, 500, map[string]string{"error": "server_error"})
			}
			return
		}

		writeJSON(w, 200, map[string]any{"orderId": orderID, "status": "cancelled"})
	})

	mux.HandleFunc("POST /support", func(w http.ResponseWriter, r *http.Request) {
		var req models.SupportRequest
		json.NewDecoder(r.Body).Decode(&req)

		if req.CustomerID == 0 {
			writeJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}

		orders, _ := s.OrdersByCustomer(r.Context(), req.CustomerID)
		ordersJSON, _ := json.Marshal(orders)

		client, _ := genai.NewClient(r.Context(), option.WithAPIKey(os.Getenv("GEMINI_API_KEY")))
		defer client.Close()

		model := client.GenerativeModel("gemini-flash-latest")

		model.Tools = []*genai.Tool{
			{
				FunctionDeclarations: []*genai.FunctionDeclaration{
					{
						Name:        "get_order_status",
						Description: "Look up the current status of an order by its id.",
						Parameters: &genai.Schema{
							Type: genai.TypeObject,
							Properties: map[string]*genai.Schema{
								"order_id": {Type: genai.TypeInteger},
							},
							Required: []string{"order_id"},
						},
					},
				},
			},
		}

		model.SystemInstruction = &genai.Content{
			Parts: []genai.Part{
				genai.Text(fmt.Sprintf(`Sen bir e-ticaret destek asistanısın. 
Kullanıcının mevcut siparişleri JSON formatında şöyledir: %s

KURALLAR:
1. Kullanıcı bir sipariş hakkında bilgi almak veya iptal etmek isterse, sipariş numarasının yukarıdaki listede olup olmadığını kontrol et.
2. Eğer sipariş numarası listede yoksa (kullanıcıya ait değilse), kullanıcıya bu siparişin kendisine ait olmadığını ve işlem yapamayacağını kibarca söyle.
3. Kullanıcı KENDİSİNE AİT bir siparişi iptal etmek isterse ASLA araç (tool) kullanma ve metin dönme! SADECE şu formatta tam bir JSON dön:
{"action": "cancel", "orderId": <num>, "summary": "İptal teklifi", "requiresConfirmation": true}`, string(ordersJSON))),
			},
		}

		session := model.StartChat()
		resp, err := session.SendMessage(r.Context(), genai.Text(req.Message))
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "ai_error"})
			return
		}

		part := resp.Candidates[0].Content.Parts[0]

		if funcCall, ok := part.(genai.FunctionCall); ok {
			if funcCall.Name == "get_order_status" {
				writeJSON(w, 200, models.SupportResponse{Answer: "Siparişiniz kargoya verilmek üzere hazırlanıyor."})
				return
			}
		}

		if txt, ok := part.(genai.Text); ok {
			var proposal models.SupportProposal
			if err := json.Unmarshal([]byte(txt), &proposal); err == nil && proposal.Action == "cancel" {
				writeJSON(w, 200, models.SupportResponse{Proposal: &proposal})
				return
			}

			writeJSON(w, 200, models.SupportResponse{Answer: string(txt)})
			return
		}
	})

	mux.HandleFunc("GET /products/{id}/recommendations", func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("id")
		productID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "gecersiz_urun_id"})
			return
		}

		recommendations, err := s.GetRecommendations(r.Context(), productID, 4)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "tavsiyeler_alinamadi"})
			return
		}

		if recommendations == nil {
			recommendations = []models.Recommendation{}
		}

		writeJSON(w, 200, recommendations)
	})
	mux.HandleFunc("GET /admin/analytics", func(w http.ResponseWriter, r *http.Request) {
		payload, err := s.GetStoreAnalytics(r.Context())
		if err != nil {
			slog.Error("analytics hatasi", "err", err)
			writeJSON(w, 500, map[string]string{"error": "analitik_verileri_alinamadi"})
			return
		}

		payloadBytes, _ := json.Marshal(payload)
		payloadJSON := string(payloadBytes)

		client, err := genai.NewClient(r.Context(), option.WithAPIKey(os.Getenv("GEMINI_API_KEY")))
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "ai_client_error"})
			return
		}
		defer client.Close()

		model := client.GenerativeModel("gemini-flash-latest")

		prompt := fmt.Sprintf(`Sen Aurora e-ticaret şirketinin deneyimli satış müdürüsün. 
    Aşağıdaki ham SQL verilerini incele. Yönetim kuruluna son 30 günün özetini veren, 
    VIP müşterilerimizi (Şampiyonlar vb.) yorumlayan ve sepet ortalamamızın sağlığını değerlendiren 
    profesyonel ama anlaşılır, en fazla 2 paragraflık bir yönetici özeti yaz.
    
    İşte Veriler: %s`, payloadJSON)

		resp, err := model.GenerateContent(r.Context(), genai.Text(prompt))
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "ai_rapor_olusturulamadi"})
			return
		}

		var geminiReport string
		if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
			if txt, ok := resp.Candidates[0].Content.Parts[0].(genai.Text); ok {
				geminiReport = string(txt)
			}
		}

		response := models.AnalyticsResponse{
			RawData:      payload,
			GeminiReport: geminiReport,
		}

		writeJSON(w, 200, response)
	})
	mux.HandleFunc("GET /admin/dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "admin/analytics.html")
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	_, _ = w.Write(b)
}
