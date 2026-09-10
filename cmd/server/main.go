package main
import "net/http"
func main(){ http.HandleFunc("/healthz",func(w http.ResponseWriter,_ *http.Request){w.WriteHeader(http.StatusNoContent)}); _=http.ListenAndServe(":8080",nil) }
