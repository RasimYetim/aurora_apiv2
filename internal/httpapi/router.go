package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rasim/aurora/internal/models"
	"github.com/rasim/aurora/internal/store"
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
		if req.CustomerID == 0 || len(req.Lines) == 0 {
			writeJSON(w, 422, map[string]string{"error": "invalid_request"})
			return
		}
		for _, l := range req.Lines {
			if l.Qty <= 0 {
				writeJSON(w, 422, map[string]string{"error": "invalid_request"})
				return
			}
		}
		idem := r.Header.Get("Idempotency-Key")

		orderID, err := s.Checkout(r.Context(), req.CustomerID, req.Lines, idem)
		switch {
		case err == nil:
			writeJSON(w, 200, map[string]int64{"orderId": orderID})
		case errors.Is(err, models.ErrOutOfStock):
			writeJSON(w, 409, map[string]string{"error": "out_of_stock"})
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
	
			model := client.GenerativeModel("gemini-1.5-flash")
	
			model.SystemInstruction = &genai.Content{
				Parts: []genai.Part{
					genai.Text(`Sen bir e-ticaret veri ayıklama asistanısın. Kullanıcının girdiği metinde eğer tarif varsa sadece ürünleri/malzemeleri ve adetlerini çıkar. Eğer bir yemek adı varsa bu yemeğin tarifinin malzemelerini ve adetlerini çıkar. Asla açıklama yapma. Çıktı SADECE şu JSON formatında bir dizi olmalı: [{"isim": "domates", "adet": 3}]`),
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
		return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
