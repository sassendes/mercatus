package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	adminhandler "mercatus/admin/handler"
	adminrepo "mercatus/admin/repository"
	adminservice "mercatus/admin/service"
	authhandler "mercatus/auth/handler"
	authrepo "mercatus/auth/repository"
	authservice "mercatus/auth/service"
	carthandler "mercatus/cart/handler"
	cartrepo "mercatus/cart/repository"
	cartservice "mercatus/cart/service"
	cataloghandler "mercatus/catalog/handler"
	catalogrepo "mercatus/catalog/repository"
	catalogservice "mercatus/catalog/service"
	gateway "mercatus/gateway/service"
	inventoryhandler "mercatus/inventory/handler"
	inventorymodel "mercatus/inventory/model"
	inventoryrepo "mercatus/inventory/repository"
	inventoryservice "mercatus/inventory/service"
	mediahandler "mercatus/media/handler"
	mediarepo "mercatus/media/repository"
	mediaservice "mercatus/media/service"
	notificationhandler "mercatus/notification/handler"
	notificationmodel "mercatus/notification/model"
	notificationrepo "mercatus/notification/repository"
	notificationservice "mercatus/notification/service"
	orderhandler "mercatus/order/handler"
	ordermodel "mercatus/order/model"
	orderrepo "mercatus/order/repository"
	orderservice "mercatus/order/service"
	paymenthandler "mercatus/payment/handler"
	paymentmodel "mercatus/payment/model"
	paymentrepo "mercatus/payment/repository"
	paymentservice "mercatus/payment/service"
	pipelinehandler "mercatus/pipeline/handler"
	pipelinerepo "mercatus/pipeline/repository"
	pipelineservice "mercatus/pipeline/service"
	"mercatus/platform/config"
	"mercatus/platform/observability"
	reviewhandler "mercatus/reviews/handler"
	reviewrepo "mercatus/reviews/repository"
	reviewservice "mercatus/reviews/service"
	sellerhandler "mercatus/seller/handler"
	sellerrepo "mercatus/seller/repository"
	sellerservice "mercatus/seller/service"
	"mercatus/web"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
)

var allModules = []string{
	"auth", "catalog", "sellers", "cart", "notifications", "reviews", "stock",
	"orders", "payments", "media", "admin", "checkout", "storefront",
}

type App struct {
	cfg      *config.Config
	db       *sql.DB
	metrics  *observability.Metrics
	ready    atomic.Bool
	limiter  *gateway.RateLimiter
	authRL   *gateway.RateLimiter
	auth     *authservice.AuthService
	jwt      *authservice.JWTManager
	catalog  *catalogservice.CatalogService
	sellers  *sellerservice.SellerService
	cart     *cartservice.CartService
	notify   *notificationservice.NotificationService
	reviews  *reviewservice.ReviewService
	stock    *inventoryservice.InventoryService
	orders   *orderservice.OrderService
	payments *paymentservice.PaymentService
	media    *mediaservice.MediaService
	admin    *adminservice.AdminService
	checkout *pipelineservice.CheckoutService
	provider *paymentservice.TestStripeProvider
}

func NewApp(cfg *config.Config, db *sql.DB) (*App, error) {
	a := &App{
		cfg:     cfg,
		db:      db,
		metrics: observability.NewMetrics(db),
		limiter: gateway.NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst),
		authRL:  gateway.NewRateLimiter(0.2, 10),
	}

	a.auth = authservice.NewAuthService(authrepo.NewUserRepository(db), authrepo.NewTokenRepository(db))
	a.jwt = authservice.NewJWTManager(cfg.JWTSecret, 15*time.Minute)

	var search catalogservice.SearchIndex
	if cfg.SearchURL != "" {
		search = catalogservice.NewHTTPSearchIndex(cfg.SearchURL, cfg.InternalToken)
	}
	a.catalog = catalogservice.NewCatalogService(catalogrepo.NewProductRepository(db), catalogrepo.NewCategoryRepository(db), search)
	a.sellers = sellerservice.NewSellerService(sellerrepo.NewSellerRepository(db))
	a.cart = cartservice.NewCartService(cartrepo.NewCartRepository(db))
	a.notify = notificationservice.NewNotificationService(notificationrepo.NewNotificationRepository(db), nil)
	a.reviews = reviewservice.NewReviewService(reviewrepo.NewReviewRepository(db))
	a.stock = inventoryservice.NewInventoryService(inventoryrepo.NewInventoryRepository(db))
	a.orders = orderservice.NewOrderService(orderrepo.NewOrderRepository(db))
	a.provider = paymentservice.NewTestStripeProvider()
	slog.Warn("payments run against the built-in test provider, no real money moves")
	a.payments = paymentservice.NewPaymentService(paymentrepo.NewPaymentRepository(db), a.provider)
	a.admin = adminservice.NewAdminService(adminrepo.NewAdminRepository(db))
	a.auth.SetCodeDelivery(func(ctx context.Context, userID int64, email, code string) {
		if _, err := a.notify.Send(ctx, &notificationmodel.SendRequest{
			UserID: userID, Channel: notificationmodel.ChannelInApp,
			Subject: "Verify your email",
			Body:    "Your Mercatus verification code is " + code + ". It expires in 24 hours.",
		}); err != nil {
			slog.WarnContext(ctx, "could not deliver verification code", "user_id", userID, "error", err)
		}
	})

	var store *mediaservice.S3
	if cfg.S3Enabled() {
		s3, err := mediaservice.NewS3(mediaservice.S3Config{
			Endpoint: cfg.S3Endpoint, Region: cfg.S3Region, Bucket: cfg.S3Bucket, AccessKey: cfg.S3AccessKey,
			SecretKey: cfg.S3SecretKey, PublicURL: cfg.S3PublicURL, PathStyle: cfg.S3PathStyle,
		})
		if err != nil {
			return nil, err
		}
		store = s3
	}
	a.media = mediaservice.NewMediaService(mediarepo.NewMediaRepository(db), store)

	sagaRepo := pipelinerepo.NewSagaRepository(db)
	a.checkout = pipelineservice.NewCheckoutService(pipelineservice.NewSagaService(sagaRepo), sagaRepo, a.checkoutDeps())
	return a, nil
}

func (a *App) checkoutDeps() pipelineservice.Deps {
	d := pipelineservice.Deps{
		CartLines: func(ctx context.Context, buyerID int64) ([]pipelineservice.Line, error) {
			c, err := a.cart.Get(ctx, buyerID)
			if err != nil {
				return nil, err
			}
			lines := make([]pipelineservice.Line, 0, len(c.Items))
			for _, it := range c.Items {
				if !it.Purchasable {
					return nil, orderservice.ErrUnavailable
				}
				lines = append(lines, pipelineservice.Line{VariantID: it.VariantID, Quantity: it.Quantity})
			}
			return lines, nil
		},
		ClearCart: a.cart.Clear,
		CreateOrder: func(ctx context.Context, buyerID int64, lines []pipelineservice.Line) (*pipelineservice.OrderSummary, error) {
			ol := make([]ordermodel.Line, len(lines))
			for i, l := range lines {
				ol[i] = ordermodel.Line{VariantID: l.VariantID, Quantity: l.Quantity}
			}
			o, err := a.orders.Create(ctx, buyerID, ol)
			if err != nil {
				return nil, err
			}
			sum := &pipelineservice.OrderSummary{ID: o.ID, TotalCents: o.TotalCents, Currency: o.Currency}
			for _, so := range o.SubOrders {
				sum.Splits = append(sum.Splits, pipelineservice.Split{
					SellerID: so.SellerID, SubtotalCents: so.SubtotalCents, CommissionCents: so.CommissionCents,
				})
			}
			return sum, nil
		},
		OrderStatus: a.orders.Status,
		CancelOrder: func(ctx context.Context, orderID int64) error {
			err := a.orders.Cancel(ctx, orderID)
			if errors.Is(err, orderservice.ErrInvalidState) {
				return nil
			}
			return err
		},
		MarkPaid: func(ctx context.Context, orderID int64) error {
			if err := a.orders.MarkPaid(ctx, orderID); err != nil {
				return err
			}
			ids, err := a.orders.SellerUserIDs(ctx, orderID)
			if err != nil {
				slog.WarnContext(ctx, "could not load sellers to notify", "order_id", orderID, "error", err)
				return nil
			}
			for _, uid := range ids {
				if _, err := a.notify.Send(ctx, &notificationmodel.SendRequest{
					UserID: uid, Channel: notificationmodel.ChannelInApp,
					Subject: fmt.Sprintf("New order #%d", orderID),
					Body:    "A buyer paid for your software. Deliver the license key or download from your Sales tab.",
				}); err != nil {
					slog.WarnContext(ctx, "could not notify seller", "user_id", uid, "error", err)
				}
			}
			return nil
		},
		Reserve: func(ctx context.Context, orderID int64, lines []pipelineservice.Line) error {
			il := make([]inventorymodel.Line, len(lines))
			for i, l := range lines {
				il[i] = inventorymodel.Line{VariantID: l.VariantID, Quantity: l.Quantity}
			}
			return a.stock.ReserveOrder(ctx, orderID, il)
		},
		Release: a.stock.ReleaseOrder,
		Commit:  a.stock.Commit,
		Restock: a.stock.RestockOrder,
		Charge: func(ctx context.Context, key string, buyerID int64, o *pipelineservice.OrderSummary) (int64, error) {
			req := &paymentmodel.ChargeRequest{IdempotencyKey: key, OrderID: o.ID, AmountCents: o.TotalCents, Currency: o.Currency}
			for _, s := range o.Splits {
				req.Splits = append(req.Splits, paymentmodel.SellerSplit{
					SellerID: s.SellerID, SubtotalCents: s.SubtotalCents, CommissionCents: s.CommissionCents,
				})
			}
			p, err := a.payments.Charge(ctx, buyerID, req)
			if p != nil {
				return p.ID, err
			}
			return 0, err
		},
		RefundOrder: a.payments.RefundOrder,
		Blocked: func(ctx context.Context, buyerID int64) (bool, error) {
			var blocked bool
			err := a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM buyer_blocks WHERE user_id = $1)`, buyerID).Scan(&blocked)
			return blocked, err
		},
		Notify: func(ctx context.Context, buyerID int64, subject, body string) {
			if _, err := a.notify.Send(ctx, &notificationmodel.SendRequest{
				UserID: buyerID, Channel: notificationmodel.ChannelInApp, Subject: subject, Body: body,
			}); err != nil {
				slog.WarnContext(ctx, "could not send notification", "user_id", buyerID, "error", err)
			}
			a.metrics.Inc("order_completed")
		},
	}
	if a.cfg.FraudURL != "" {
		fc := newFraudClient(a.cfg.FraudURL, a.cfg.InternalToken)
		failOpen := a.cfg.FraudFailOpen
		d.Fraud = func(ctx context.Context, buyerID, amount int64) (pipelineservice.FraudDecision, error) {
			newAccount := false
			if u, err := a.auth.Me(ctx, buyerID); err == nil {
				newAccount = time.Since(u.CreatedAt) < 7*24*time.Hour
			}
			decision, err := fc.Score(ctx, buyerID, amount, newAccount, pipelineservice.CountryFrom(ctx))
			if err != nil {
				a.metrics.Inc("fraud_check_error")
				if failOpen {
					return pipelineservice.FraudApprove, err
				}
				return pipelineservice.FraudDecline, err
			}
			a.metrics.Inc("fraud_" + string(decision))
			return decision, nil
		}
	}
	return d
}

func enabled(only []string, module string) bool {
	return len(only) == 0 || slices.Contains(only, module)
}

func (a *App) Router() (http.Handler, error) {
	for _, m := range a.cfg.Only {
		if !slices.Contains(allModules, m) {
			return nil, fmt.Errorf("unknown module %q in --only (valid: %s)", m, strings.Join(allModules, ","))
		}
	}
	only := a.cfg.Only
	secret := a.cfg.JWTSecret

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	if a.cfg.TrustProxyHeaders {
		r.Use(chimw.RealIP)
	}
	r.Use(observability.RequestLogger, chimw.Recoverer, a.metrics.Middleware, gateway.SecurityHeaders(a.cfg.TrustProxyHeaders))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	r.Get("/readyz", a.readyz)

	r.Route("/api", func(api chi.Router) {
		api.Use(gateway.CORS(a.cfg.CORSOrigins), gateway.MaxBody(a.cfg.MaxBodyBytes), a.limiter.Middleware)
		api.Use(chimw.Timeout(a.cfg.WriteTimeout - time.Second))
		mount := func(module, prefix string, h http.Handler) {
			if enabled(only, module) {
				api.Mount(prefix, h)
			}
		}
		if enabled(only, "auth") {
			ah := authhandler.NewAuthHandler(a.auth, a.jwt, secret).Routes()
			api.With(a.authLimit).Mount("/auth", ah)
		}
		mount("catalog", "/catalog", cataloghandler.NewCatalogHandler(a.catalog, secret).Routes())
		mount("sellers", "/sellers", sellerhandler.NewSellerHandler(a.sellers, secret).Routes())
		mount("cart", "/cart", carthandler.NewCartHandler(a.cart, secret).Routes())
		mount("notifications", "/notifications", notificationhandler.NewNotificationHandler(a.notify, secret).Routes())
		mount("reviews", "/reviews", reviewhandler.NewReviewHandler(a.reviews, secret).Routes())
		mount("stock", "/stock", inventoryhandler.NewInventoryHandler(a.stock, secret).Routes())
		mount("orders", "/orders", orderhandler.NewOrderHandler(a.orders, secret).Routes())
		mount("payments", "/payments", paymenthandler.NewPaymentHandler(a.payments, secret).Routes())
		mount("media", "/media", mediahandler.NewMediaHandler(a.media, secret).Routes())
		mount("admin", "/admin", adminhandler.NewAdminHandler(a.admin, secret).Routes())
		mount("checkout", "/checkout", a.withCountry(pipelinehandler.NewPipelineHandler(a.checkout, secret).Routes()))
		api.NotFound(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		})
		api.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`{"error":"method not allowed"}`))
		})
	})

	if enabled(only, "storefront") {
		spa := gateway.NewSPA(web.Files())
		if !spa.Available() {
			slog.Warn("storefront assets missing from this build; only the API will be served")
		}
		r.Handle("/*", spa)
	}
	return r, nil
}

func (a *App) withCountry(next http.Handler) http.Handler {
	header := a.cfg.CountryHeader
	if header == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := strings.ToUpper(strings.TrimSpace(r.Header.Get(header)))
		if len(c) == 2 {
			r = r.WithContext(pipelineservice.WithCountry(r.Context(), c))
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) authLimit(next http.Handler) http.Handler {
	strict := a.authRL.Middleware(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/login") || strings.HasSuffix(r.URL.Path, "/register") ||
			strings.HasSuffix(r.URL.Path, "/verify-email")) {
			strict.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) readyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !a.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"shutting_down"}`))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.db.PingContext(ctx); err != nil {
		slog.WarnContext(r.Context(), "readiness check failed", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"database_unavailable"}`))
		return
	}
	_, _ = w.Write([]byte(`{"status":"ready"}`))
}

func every(ctx context.Context, name string, interval time.Duration, fn func(context.Context) error) {
	if interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			runCtx, cancel := context.WithTimeout(ctx, interval)
			if err := fn(runCtx); err != nil && ctx.Err() == nil {
				slog.Warn("background job failed", "job", name, "error", err)
			}
			cancel()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

func (a *App) StartWorkers(ctx context.Context) {
	go a.limiter.Run(ctx)
	go a.authRL.Run(ctx)
	if !a.cfg.RunBackgroundWorkers {
		slog.Info("background workers disabled")
		return
	}
	every(ctx, "expire-reservations", a.cfg.SweepInterval, func(ctx context.Context) error {
		n, err := a.stock.ExpireStale(ctx)
		if n > 0 {
			slog.Info("released stale reservations", "count", n)
		}
		return err
	})
	every(ctx, "recover-sagas", a.cfg.SweepInterval, func(ctx context.Context) error {
		n, err := a.checkout.RecoverStale(ctx, 2*time.Minute)
		if n > 0 {
			slog.Warn("recovered interrupted checkouts", "count", n)
		}
		return err
	})
	every(ctx, "prune-refresh-tokens", time.Hour, func(ctx context.Context) error {
		_, err := a.auth.PruneTokens(ctx)
		return err
	})
	if a.cfg.SearchURL != "" {
		every(ctx, "search-reindex", a.cfg.ReindexInterval, func(ctx context.Context) error {
			n, err := a.catalog.Reindex(ctx)
			if err == nil {
				slog.Debug("search index refreshed", "documents", n)
			}
			return err
		})
	}
}
