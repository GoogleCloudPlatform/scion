# Project-Specific Patterns

This document outlines patterns used across internal Go projects for consistency.

## Web Servers and HTTP Routing

When building a web server or HTTP API, you **MUST** adhere to the following routing and server constraints:
- Use the built-in standard library `net/http` package.
- Or, use the `github.com/gorilla/mux` and `github.com/gorilla/handlers` packages for advanced routing and middleware needs.
- **Do NOT** use other third-party HTTP server frameworks (e.g., Gin, Echo, Fiber, Chi). The goal is to rely on the standard library interface (`http.Handler`) to keep handlers clean, standard, and highly portable.

```go
// Example using gorilla/mux
func NewRouter(store *UserStore) *mux.Router {
    r := mux.NewRouter()
    r.HandleFunc("/users", HandleCreateUser(store)).Methods(http.MethodPost)
    
    // Example using gorilla/handlers for middleware
    // loggedRouter := handlers.LoggingHandler(os.Stdout, r)
    return r
}
```

## Separation of Concerns (Data Access)

To ensure code is testable without requiring complex database mocking or tightly coupled logic, separate data ingestion and domain logic from database transactions.

### Domain Models and Data Structures
Define your data structures clearly.

```go
type User struct {
    ID    string
    Name  string
    Email string
}
```

### Database Service/Store
Instead of embedding database calls directly into HTTP handlers or complex business logic flows, create a dedicated struct (service/store) responsible for database operations. This struct accepts the domain models.

```go
// UserStore handles database operations for the User model.
type UserStore struct {
    db *sql.DB
}

func NewUserStore(db *sql.DB) *UserStore {
    return &UserStore{db: db}
}

// Save inserts or updates a user in the database.
// Notice it accepts the domain model, not an HTTP request.
func (s *UserStore) Save(ctx context.Context, user *User) error {
    _, err := s.db.ExecContext(ctx, "INSERT INTO users (id, name, email) VALUES ($1, $2, $3)", user.ID, user.Name, user.Email)
    return err
}

func (s *UserStore) GetByID(ctx context.Context, id string) (*User, error) {
    // ... DB query logic ...
}
```

### Business Logic / Handlers
The handler or core logic parses input, builds the domain models, and then passes them to the store. This allows you to test the parsing and business logic independently of the database.

```go
func HandleCreateUser(store *UserStore) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // 1. Ingest Data (e.g., decode JSON)
        var input struct {
            Name  string
            Email string
        }
        if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
            http.Error(w, err.Error(), http.StatusBadRequest)
            return
        }

        // 2. Build Domain Model
        user := &User{
            ID:    uuid.New().String(),
            Name:  input.Name,
            Email: input.Email,
        }

        // 3. Delegate to Database Service
        if err := store.Save(r.Context(), user); err != nil {
            http.Error(w, "Failed to save user", http.StatusInternalServerError)
            return
        }

        w.WriteHeader(http.StatusCreated)
    }
}
```
**Why this helps:** You can test `HandleCreateUser` by passing a `UserStore` connected to a test database or an in-memory SQLite instance, or you can extract the validation/building logic into a pure function that requires no DB at all.

## Configuration Management

### Centralized Config Struct
Define a `Config` struct in `config/config.go` that maps to project settings.

```go
type Config struct {
    Port         int
    DbConnection []dbconnection
    Redis        redis
    LogLevel     string
    // ...
}
```

### Loading from TOML
Use `github.com/BurntSushi/toml` to load configuration from a `.env` file (which is actually TOML).

```go
func Load() (config Config, err error) {
    var configFile = ".env"
    // Prefer absolute path if available, fallback to local
    if _, err = toml.DecodeFile(configFile, &config); err != nil {
        return config, err
    }
    return config, nil
}
```

## Logging Patterns

### Structured Logging (JSON)
For performance tracking and request auditing, use structured JSON logging. Timings should be in microseconds (`μs`).

```go
type ResponseBreakdown struct {
    Name        string `json:"name"`
    CurrentTime int64  `json:"current_time"`
    TimeUnits   string `json:"time_units"`
    RequestID   string `json:"request_id"`
    Total       int64  `json:"total_time_us"`
    // ...
}
```

### logrus Integration
Use `logrus` for general application logging, with levels configurable via the `Config` struct.

```go
func SetupLogging(logLevel string) {
    level, _ := logrus.ParseLevel(logLevel)
    logrus.SetLevel(level)
    logrus.SetFormatter(&logrus.JSONFormatter{}) // Optional: matches structured output
}
```
