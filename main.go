package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	ctx        = context.Background()
	rdb        *redis.Client
	userSvcURL string
	client     = &http.Client{Timeout: 5 * time.Second}
)

type Note struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	userSvcURL = getEnv("USER_SVC_URL", "http://localhost:8081")
	rdb = redis.NewClient(&redis.Options{
		Addr: getEnv("REDIS_URL", "localhost:6379"),
	})

	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("WARNING: Redis not available: %v", err)
	} else {
		log.Println("Connected to Redis")
	}

	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/notes", notesHandler)
	http.HandleFunc("/notes/", noteByIDHandler)

	port := getEnv("PORT", "8082")
	log.Printf("note-svc starting on :%s (user-svc: %s)", port, userSvcURL)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"service": "note-svc",
	})
}

func notesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodPost:
		createNote(w, r)
	case http.MethodGet:
		listNotes(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func noteByIDHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := strings.TrimPrefix(r.URL.Path, "/notes/")
	if id == "" {
		http.Error(w, "missing note id", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		getNote(w, id)
	case http.MethodDelete:
		deleteNote(w, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// verifyUser checks if a user exists by calling user-svc
func verifyUser(userID string) bool {
	resp, err := client.Get(userSvcURL + "/users/" + userID)
	if err != nil {
		log.Printf("Failed to verify user %s: %v", userID, err)
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func createNote(w http.ResponseWriter, r *http.Request) {
	var note Note
	if err := json.NewDecoder(r.Body).Decode(&note); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if note.UserID == "" || note.Title == "" {
		http.Error(w, "user_id and title required", http.StatusBadRequest)
		return
	}

	// Verify user exists (service-to-service call)
	if !verifyUser(note.UserID) {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	note.ID = fmt.Sprintf("note_%d", time.Now().UnixNano())
	note.CreatedAt = time.Now().UTC().Format(time.RFC3339)

	data, _ := json.Marshal(note)
	rdb.Set(ctx, "note:"+note.ID, data, 0)
	rdb.SAdd(ctx, "notes:user:"+note.UserID, note.ID)
	rdb.SAdd(ctx, "notes", note.ID)

	log.Printf("Created note: %s for user %s", note.ID, note.UserID)
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(note)
}

func getNote(w http.ResponseWriter, id string) {
	data, err := rdb.Get(ctx, "note:"+id).Result()
	if err == redis.Nil {
		http.Error(w, "note not found", http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Write([]byte(data))
}

func listNotes(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")

	var ids []string
	var err error

	if userID != "" {
		ids, err = rdb.SMembers(ctx, "notes:user:"+userID).Result()
	} else {
		ids, err = rdb.SMembers(ctx, "notes").Result()
	}

	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	notes := []Note{}
	for _, id := range ids {
		data, err := rdb.Get(ctx, "note:"+id).Result()
		if err != nil {
			continue
		}
		var note Note
		json.Unmarshal([]byte(data), &note)
		notes = append(notes, note)
	}

	json.NewEncoder(w).Encode(notes)
}

func deleteNote(w http.ResponseWriter, id string) {
	// Get note to find user_id for cleanup
	data, err := rdb.Get(ctx, "note:"+id).Result()
	if err == nil {
		var note Note
		json.Unmarshal([]byte(data), &note)
		rdb.SRem(ctx, "notes:user:"+note.UserID, id)
	}

	rdb.Del(ctx, "note:"+id)
	rdb.SRem(ctx, "notes", id)
	log.Printf("Deleted note: %s", id)
	w.WriteHeader(http.StatusNoContent)
}
