package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

type Config struct {
	Addr          string
	DatabaseURL   string
	SessionSecret string
	USDAAPIKey    string
	AppHost       string
	SecureCookies bool
	Timezone      string
}

type App struct {
	cfg       Config
	db        *pgxpool.Pool
	location  *time.Location
	templates string
}

type contextKey string

const userContextKey contextKey = "user"

func main() {
	cfg := loadConfig()
	if len(os.Args) > 1 && os.Args[1] == "create-user" {
		if err := runCreateUser(cfg, os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		db, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
		must(err)
		defer db.Close()
		must(migrate(context.Background(), db))
		log.Println("migrations complete")
		return
	}

	app, err := newApp(cfg)
	must(err)
	defer app.db.Close()
	must(migrate(context.Background(), app.db))

	r := app.routes()
	log.Printf("foodtrack listening on %s", cfg.Addr)
	must(http.ListenAndServe(cfg.Addr, r))
}

func loadConfig() Config {
	cfg := Config{
		Addr:          env("ADDR", ":8080"),
		DatabaseURL:   env("DATABASE_URL", "postgres://foodtrack:foodtrack@localhost:5432/foodtrack?sslmode=disable"),
		SessionSecret: env("SESSION_SECRET", ""),
		USDAAPIKey:    env("USDA_API_KEY", "DEMO_KEY"),
		AppHost:       env("APP_HOST", "localhost"),
		Timezone:      env("APP_TIMEZONE", "Europe/London"),
	}
	cfg.SecureCookies = strings.ToLower(env("SECURE_COOKIES", "true")) != "false"
	if cfg.SessionSecret == "" {
		log.Println("WARNING: SESSION_SECRET is empty; generating an ephemeral secret. Set SESSION_SECRET in production.")
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		cfg.SessionSecret = hex.EncodeToString(b)
	}
	return cfg
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newApp(cfg Config) (*App, error) {
	db, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &App{cfg: cfg, db: db, location: loc, templates: "templates"}, nil
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func migrate(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, `
CREATE TABLE IF NOT EXISTS users (
  id BIGSERIAL PRIMARY KEY,
  email TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  display_name TEXT NOT NULL,
  daily_calorie_target DOUBLE PRECISION NOT NULL DEFAULT 2000,
  daily_protein_target_g DOUBLE PRECISION NOT NULL DEFAULT 150,
  daily_carbs_target_g DOUBLE PRECISION NOT NULL DEFAULT 220,
  daily_fat_target_g DOUBLE PRECISION NOT NULL DEFAULT 70,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS meals (
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  eaten_at TIMESTAMPTZ NOT NULL,
  description TEXT NOT NULL,
  source TEXT NOT NULL CHECK (source IN ('usda_lookup', 'manual')),
  usda_food_id BIGINT NOT NULL DEFAULT 0,
  quantity_grams DOUBLE PRECISION NOT NULL DEFAULT 0,
  calories DOUBLE PRECISION NOT NULL DEFAULT 0,
  protein_g DOUBLE PRECISION NOT NULL DEFAULT 0,
  carbs_g DOUBLE PRECISION NOT NULL DEFAULT 0,
  fat_g DOUBLE PRECISION NOT NULL DEFAULT 0,
  notes TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS meals_user_eaten_idx ON meals(user_id, eaten_at DESC);

CREATE TABLE IF NOT EXISTS food_cache (
  id BIGSERIAL PRIMARY KEY,
  query_text TEXT NOT NULL,
  usda_food_id BIGINT,
  nutrient_json JSONB NOT NULL,
  cached_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`)
	return err
}

func runCreateUser(cfg Config, args []string) error {
	fs := flag.NewFlagSet("create-user", flag.ExitOnError)
	email := fs.String("email", "", "email address")
	password := fs.String("password", env("FOODTRACK_NEW_USER_PASSWORD", ""), "password, or set FOODTRACK_NEW_USER_PASSWORD")
	name := fs.String("name", "", "display name")
	calories := fs.Float64("calories", 2000, "daily calorie target")
	protein := fs.Float64("protein", 150, "daily protein target in grams")
	carbs := fs.Float64("carbs", 220, "daily carbs target in grams")
	fat := fs.Float64("fat", 70, "daily fat target in grams")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" || *password == "" {
		return errors.New("email and password are required")
	}
	if *name == "" {
		*name = strings.Split(*email, "@")[0]
	}
	db, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := migrate(context.Background(), db); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(*password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = db.Exec(context.Background(), `
INSERT INTO users (email, password_hash, display_name, daily_calorie_target, daily_protein_target_g, daily_carbs_target_g, daily_fat_target_g)
VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (email) DO UPDATE SET
  password_hash = EXCLUDED.password_hash,
  display_name = EXCLUDED.display_name,
  daily_calorie_target = EXCLUDED.daily_calorie_target,
  daily_protein_target_g = EXCLUDED.daily_protein_target_g,
  daily_carbs_target_g = EXCLUDED.daily_carbs_target_g,
  daily_fat_target_g = EXCLUDED.daily_fat_target_g
`, strings.ToLower(strings.TrimSpace(*email)), string(hash), *name, *calories, *protein, *carbs, *fat)
	if err != nil {
		return err
	}
	log.Printf("user %s created/updated", *email)
	return nil
}

type User struct {
	ID             int64
	Email          string
	PasswordHash   string
	DisplayName    string
	TargetCalories float64
	TargetProtein  float64
	TargetCarbs    float64
	TargetFat      float64
}

type Meal struct {
	ID            int64
	UserID        int64
	EatenAt       time.Time
	Description   string
	Source        string
	USDAFoodID    int64
	QuantityGrams float64
	Calories      float64
	Protein       float64
	Carbs         float64
	Fat           float64
	Notes         string
}

type Totals struct {
	Calories float64
	Protein  float64
	Carbs    float64
	Fat      float64
}

func (a *App) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(a.securityHeaders)
	r.Use(a.withUser)
	r.Get("/static/*", func(w http.ResponseWriter, r *http.Request) {
		http.StripPrefix("/static/", http.FileServer(http.Dir("static"))).ServeHTTP(w, r)
	})
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	r.Get("/login", a.loginForm)
	r.Post("/login", a.loginPost)
	r.Group(func(pr chi.Router) {
		pr.Use(a.requireAuth)
		pr.Get("/", a.today)
		pr.Get("/day/{date}", a.day)
		pr.Get("/settings", a.settingsForm)
		pr.Post("/settings", a.settingsPost)
		pr.Get("/meals/new", a.newMealForm)
		pr.Post("/meals/manual", a.createManualMeal)
		pr.Post("/meals/usda", a.createUSDAMeal)
		pr.Get("/api/usda/search", a.usdaSearch)
		pr.Get("/meals/{id}/edit", a.editMealForm)
		pr.Post("/meals/{id}/update", a.updateMeal)
		pr.Post("/meals/{id}/delete", a.deleteMeal)
		pr.Post("/logout", a.logout)
	})
	return r
}

func (a *App) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (a *App) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, ok := a.readSession(r)
		if ok {
			user, err := a.getUserByID(r.Context(), uid)
			if err == nil {
				ctx := context.WithValue(r.Context(), userContextKey, user)
				r = r.WithContext(ctx)
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r) == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func currentUser(r *http.Request) *User {
	v := r.Context().Value(userContextKey)
	if v == nil {
		return nil
	}
	user, _ := v.(*User)
	return user
}

func (a *App) getUserByID(ctx context.Context, id int64) (*User, error) {
	row := a.db.QueryRow(ctx, `SELECT id,email,password_hash,display_name,daily_calorie_target,daily_protein_target_g,daily_carbs_target_g,daily_fat_target_g FROM users WHERE id=$1`, id)
	return scanUser(row)
}

func (a *App) getUserByEmail(ctx context.Context, email string) (*User, error) {
	row := a.db.QueryRow(ctx, `SELECT id,email,password_hash,display_name,daily_calorie_target,daily_protein_target_g,daily_carbs_target_g,daily_fat_target_g FROM users WHERE email=$1`, strings.ToLower(strings.TrimSpace(email)))
	return scanUser(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(row rowScanner) (*User, error) {
	var u User
	if err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.TargetCalories, &u.TargetProtein, &u.TargetCarbs, &u.TargetFat); err != nil {
		return nil, err
	}
	return &u, nil
}

func (a *App) loginForm(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, r, "login.html", map[string]any{"Title": "Log in"})
}

func (a *App) loginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.badRequest(w, "Invalid form")
		return
	}
	u, err := a.getUserByEmail(r.Context(), r.FormValue("email"))
	if err != nil || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(r.FormValue("password"))) != nil {
		a.render(w, r, "login.html", map[string]any{"Title": "Log in", "Error": "Email or password was not recognised."})
		return
	}
	a.writeSession(w, u.ID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "ft_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.cfg.SecureCookies})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *App) writeSession(w http.ResponseWriter, userID int64) {
	exp := time.Now().Add(30 * 24 * time.Hour).Unix()
	payload := fmt.Sprintf("%d|%d", userID, exp)
	sig := a.sign(payload)
	http.SetCookie(w, &http.Cookie{
		Name:     "ft_session",
		Value:    payload + "|" + sig,
		Path:     "/",
		MaxAge:   30 * 24 * 60 * 60,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.cfg.SecureCookies,
	})
}

func (a *App) readSession(r *http.Request) (int64, bool) {
	c, err := r.Cookie("ft_session")
	if err != nil || c.Value == "" {
		return 0, false
	}
	parts := strings.Split(c.Value, "|")
	if len(parts) != 3 {
		return 0, false
	}
	payload := parts[0] + "|" + parts[1]
	if !hmac.Equal([]byte(parts[2]), []byte(a.sign(payload))) {
		return 0, false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return 0, false
	}
	uid, err := strconv.ParseInt(parts[0], 10, 64)
	return uid, err == nil
}

func (a *App) sign(payload string) string {
	mac := hmac.New(sha256.New, []byte(a.cfg.SessionSecret))
	_, _ = mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func (a *App) today(w http.ResponseWriter, r *http.Request) {
	date := time.Now().In(a.location).Format("2006-01-02")
	http.Redirect(w, r, "/day/"+date, http.StatusSeeOther)
}

func (a *App) day(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	dateStr := chi.URLParam(r, "date")
	date, err := time.ParseInLocation("2006-01-02", dateStr, a.location)
	if err != nil {
		a.badRequest(w, "Use dates as YYYY-MM-DD")
		return
	}
	meals, totals, err := a.mealsForDay(r.Context(), user.ID, date)
	if err != nil {
		a.serverError(w, err)
		return
	}
	data := map[string]any{
		"Title":    "Daily log",
		"Date":     date,
		"PrevDate": date.AddDate(0, 0, -1).Format("2006-01-02"),
		"NextDate": date.AddDate(0, 0, 1).Format("2006-01-02"),
		"Meals":    meals,
		"Totals":   totals,
		"User":     user,
	}
	a.render(w, r, "day.html", data)
}

func (a *App) mealsForDay(ctx context.Context, userID int64, day time.Time) ([]Meal, Totals, error) {
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, a.location)
	end := start.AddDate(0, 0, 1)
	rows, err := a.db.Query(ctx, `SELECT id,user_id,eaten_at,description,source,usda_food_id,quantity_grams,calories,protein_g,carbs_g,fat_g,notes FROM meals WHERE user_id=$1 AND eaten_at >= $2 AND eaten_at < $3 ORDER BY eaten_at DESC, id DESC`, userID, start, end)
	if err != nil {
		return nil, Totals{}, err
	}
	defer rows.Close()
	var meals []Meal
	var totals Totals
	for rows.Next() {
		m, err := scanMeal(rows)
		if err != nil {
			return nil, Totals{}, err
		}
		meals = append(meals, m)
		totals.Calories += m.Calories
		totals.Protein += m.Protein
		totals.Carbs += m.Carbs
		totals.Fat += m.Fat
	}
	return meals, totals, rows.Err()
}

func scanMeal(row rowScanner) (Meal, error) {
	var m Meal
	if err := row.Scan(&m.ID, &m.UserID, &m.EatenAt, &m.Description, &m.Source, &m.USDAFoodID, &m.QuantityGrams, &m.Calories, &m.Protein, &m.Carbs, &m.Fat, &m.Notes); err != nil {
		return m, err
	}
	return m, nil
}

func (a *App) settingsForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "settings.html", map[string]any{"Title": "Targets", "User": currentUser(r)})
}

func (a *App) settingsPost(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if err := r.ParseForm(); err != nil {
		a.badRequest(w, "Invalid form")
		return
	}
	calories := parseFloat(r.FormValue("calories"))
	protein := parseFloat(r.FormValue("protein"))
	carbs := parseFloat(r.FormValue("carbs"))
	fat := parseFloat(r.FormValue("fat"))
	_, err := a.db.Exec(r.Context(), `UPDATE users SET daily_calorie_target=$1,daily_protein_target_g=$2,daily_carbs_target_g=$3,daily_fat_target_g=$4 WHERE id=$5`, calories, protein, carbs, fat, user.ID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (a *App) newMealForm(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Title":        "Add meal",
		"NowLocal":     time.Now().In(a.location).Format("2006-01-02T15:04"),
		"DefaultGrams": "100",
	}
	a.render(w, r, "new_meal.html", data)
}

func (a *App) createManualMeal(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if err := r.ParseForm(); err != nil {
		a.badRequest(w, "Invalid form")
		return
	}
	eatenAt, err := a.parseLocalDateTime(r.FormValue("eaten_at"))
	if err != nil {
		a.badRequest(w, "Invalid meal time")
		return
	}
	desc := strings.TrimSpace(r.FormValue("description"))
	if desc == "" {
		a.badRequest(w, "Description is required")
		return
	}
	_, err = a.db.Exec(r.Context(), `INSERT INTO meals (user_id,eaten_at,description,source,calories,protein_g,carbs_g,fat_g,notes) VALUES ($1,$2,$3,'manual',$4,$5,$6,$7,$8)`, user.ID, eatenAt, desc, parseFloat(r.FormValue("calories")), parseFloat(r.FormValue("protein")), parseFloat(r.FormValue("carbs")), parseFloat(r.FormValue("fat")), strings.TrimSpace(r.FormValue("notes")))
	if err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/day/"+eatenAt.In(a.location).Format("2006-01-02"), http.StatusSeeOther)
}

func (a *App) createUSDAMeal(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if err := r.ParseForm(); err != nil {
		a.badRequest(w, "Invalid form")
		return
	}
	eatenAt, err := a.parseLocalDateTime(r.FormValue("eaten_at"))
	if err != nil {
		a.badRequest(w, "Invalid meal time")
		return
	}
	fdcID, err := strconv.ParseInt(r.FormValue("fdc_id"), 10, 64)
	if err != nil || fdcID <= 0 {
		a.badRequest(w, "Choose a USDA food first")
		return
	}
	grams := parseFloat(r.FormValue("quantity_grams"))
	if grams <= 0 {
		grams = 100
	}
	food, err := a.fetchFoodDetails(r.Context(), fdcID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	n := food.Nutrients.Scale(grams)
	desc := food.Description
	if custom := strings.TrimSpace(r.FormValue("description")); custom != "" {
		desc = custom + " (USDA: " + food.Description + ")"
	}
	_, err = a.db.Exec(r.Context(), `INSERT INTO meals (user_id,eaten_at,description,source,usda_food_id,quantity_grams,calories,protein_g,carbs_g,fat_g,notes) VALUES ($1,$2,$3,'usda_lookup',$4,$5,$6,$7,$8,$9,$10)`, user.ID, eatenAt, desc, fdcID, grams, n.Calories, n.Protein, n.Carbs, n.Fat, strings.TrimSpace(r.FormValue("notes")))
	if err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/day/"+eatenAt.In(a.location).Format("2006-01-02"), http.StatusSeeOther)
}

func (a *App) editMealForm(w http.ResponseWriter, r *http.Request) {
	meal, err := a.getMealForUser(r.Context(), currentUser(r).ID, idParam(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data := map[string]any{"Title": "Edit meal", "Meal": meal, "EatenLocal": meal.EatenAt.In(a.location).Format("2006-01-02T15:04")}
	a.render(w, r, "edit_meal.html", data)
}

func (a *App) updateMeal(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	mealID := idParam(r)
	if err := r.ParseForm(); err != nil {
		a.badRequest(w, "Invalid form")
		return
	}
	eatenAt, err := a.parseLocalDateTime(r.FormValue("eaten_at"))
	if err != nil {
		a.badRequest(w, "Invalid meal time")
		return
	}
	_, err = a.db.Exec(r.Context(), `UPDATE meals SET eaten_at=$1,description=$2,source='manual',usda_food_id=0,quantity_grams=0,calories=$3,protein_g=$4,carbs_g=$5,fat_g=$6,notes=$7,updated_at=now() WHERE id=$8 AND user_id=$9`, eatenAt, strings.TrimSpace(r.FormValue("description")), parseFloat(r.FormValue("calories")), parseFloat(r.FormValue("protein")), parseFloat(r.FormValue("carbs")), parseFloat(r.FormValue("fat")), strings.TrimSpace(r.FormValue("notes")), mealID, user.ID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/day/"+eatenAt.In(a.location).Format("2006-01-02"), http.StatusSeeOther)
}

func (a *App) deleteMeal(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	mealID := idParam(r)
	meal, err := a.getMealForUser(r.Context(), user.ID, mealID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_, err = a.db.Exec(r.Context(), `DELETE FROM meals WHERE id=$1 AND user_id=$2`, mealID, user.ID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/day/"+meal.EatenAt.In(a.location).Format("2006-01-02"), http.StatusSeeOther)
}

func (a *App) getMealForUser(ctx context.Context, userID, mealID int64) (Meal, error) {
	row := a.db.QueryRow(ctx, `SELECT id,user_id,eaten_at,description,source,usda_food_id,quantity_grams,calories,protein_g,carbs_g,fat_g,notes FROM meals WHERE id=$1 AND user_id=$2`, mealID, userID)
	return scanMeal(row)
}

func idParam(r *http.Request) int64 {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id
}

func (a *App) parseLocalDateTime(s string) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return time.Now(), nil
	}
	return time.ParseInLocation("2006-01-02T15:04", s, a.location)
}

func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0
	}
	return f
}

func (a *App) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	if _, ok := data["CurrentUser"]; !ok {
		data["CurrentUser"] = currentUser(r)
	}
	if _, ok := data["AppHost"]; !ok {
		data["AppHost"] = a.cfg.AppHost
	}
	funcs := template.FuncMap{
		"f0": func(v float64) string { return fmt.Sprintf("%.0f", v) },
		"f1": func(v float64) string { return fmt.Sprintf("%.1f", v) },
		"pct": func(actual, target float64) int {
			if target <= 0 {
				return 0
			}
			p := int(math.Round((actual / target) * 100))
			if p < 0 {
				return 0
			}
			return p
		},
		"bar": func(actual, target float64) int {
			if target <= 0 {
				return 0
			}
			p := int(math.Round((actual / target) * 100))
			if p > 100 {
				return 100
			}
			if p < 0 {
				return 0
			}
			return p
		},
		"localtime": func(t time.Time) string { return t.In(a.location).Format("15:04") },
		"datefmt":   func(t time.Time) string { return t.In(a.location).Format("Mon 02 Jan 2006") },
	}
	t, err := template.New("base.html").Funcs(funcs).ParseFiles(a.templates+"/base.html", a.templates+"/"+page)
	if err != nil {
		a.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "base", data); err != nil {
		log.Println("template error:", err)
	}
}

func (a *App) badRequest(w http.ResponseWriter, msg string) {
	http.Error(w, msg, http.StatusBadRequest)
}

func (a *App) serverError(w http.ResponseWriter, err error) {
	log.Println("server error:", err)
	http.Error(w, "Something went wrong", http.StatusInternalServerError)
}

type Nutrients struct {
	Calories float64 `json:"calories"`
	Protein  float64 `json:"protein"`
	Carbs    float64 `json:"carbs"`
	Fat      float64 `json:"fat"`
}

func (n Nutrients) Scale(grams float64) Nutrients {
	factor := grams / 100.0
	return Nutrients{Calories: n.Calories * factor, Protein: n.Protein * factor, Carbs: n.Carbs * factor, Fat: n.Fat * factor}
}

type FoodCandidate struct {
	FDCID       int64     `json:"fdc_id"`
	Description string    `json:"description"`
	BrandOwner  string    `json:"brand_owner,omitempty"`
	DataType    string    `json:"data_type"`
	Nutrients   Nutrients `json:"nutrients_per_100g"`
}

type FoodDetails struct {
	FDCID       int64
	Description string
	Nutrients   Nutrients
}

func (a *App) usdaSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		a.writeJSON(w, []FoodCandidate{})
		return
	}
	foods, err := a.searchFoods(r.Context(), q)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.writeJSON(w, foods)
}

func (a *App) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func (a *App) searchFoods(ctx context.Context, query string) ([]FoodCandidate, error) {
	endpoint := "https://api.nal.usda.gov/fdc/v1/foods/search?api_key=" + url.QueryEscape(a.cfg.USDAAPIKey)
	body := map[string]any{"query": query, "pageSize": 8}
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("usda search failed: %s", resp.Status)
	}
	var raw struct {
		Foods []struct {
			FDCID         int64  `json:"fdcId"`
			Description   string `json:"description"`
			BrandOwner    string `json:"brandOwner"`
			DataType      string `json:"dataType"`
			FoodNutrients []struct {
				NutrientID   int     `json:"nutrientId"`
				NutrientName string  `json:"nutrientName"`
				Value        float64 `json:"value"`
			} `json:"foodNutrients"`
		} `json:"foods"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]FoodCandidate, 0, len(raw.Foods))
	for _, f := range raw.Foods {
		n := Nutrients{}
		for _, x := range f.FoodNutrients {
			applyNutrient(&n, x.NutrientID, x.NutrientName, x.Value)
		}
		out = append(out, FoodCandidate{FDCID: f.FDCID, Description: strings.TrimSpace(f.Description), BrandOwner: f.BrandOwner, DataType: f.DataType, Nutrients: n})
	}
	return out, nil
}

func (a *App) fetchFoodDetails(ctx context.Context, fdcID int64) (FoodDetails, error) {
	endpoint := fmt.Sprintf("https://api.nal.usda.gov/fdc/v1/food/%d?api_key=%s", fdcID, url.QueryEscape(a.cfg.USDAAPIKey))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return FoodDetails{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return FoodDetails{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return FoodDetails{}, fmt.Errorf("usda detail failed: %s", resp.Status)
	}
	var raw struct {
		FDCID         int64  `json:"fdcId"`
		Description   string `json:"description"`
		FoodNutrients []struct {
			Amount   float64 `json:"amount"`
			Nutrient struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
				Unit string `json:"unitName"`
			} `json:"nutrient"`
		} `json:"foodNutrients"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return FoodDetails{}, err
	}
	n := Nutrients{}
	for _, x := range raw.FoodNutrients {
		applyNutrient(&n, x.Nutrient.ID, x.Nutrient.Name, x.Amount)
	}
	return FoodDetails{FDCID: raw.FDCID, Description: raw.Description, Nutrients: n}, nil
}

func applyNutrient(n *Nutrients, id int, name string, value float64) {
	lower := strings.ToLower(name)
	switch {
	case id == 1008 || strings.Contains(lower, "energy"):
		if n.Calories == 0 {
			n.Calories = value
		}
	case id == 1003 || strings.Contains(lower, "protein"):
		n.Protein = value
	case id == 1005 || strings.Contains(lower, "carbohydrate"):
		n.Carbs = value
	case id == 1004 || strings.Contains(lower, "total lipid") || strings.Contains(lower, "fat"):
		n.Fat = value
	}
}
