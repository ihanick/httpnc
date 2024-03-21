package main

import (
	"bytes"
	"crypto/tls"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"strconv"
	"sync/atomic"
	"time"
)


var stop = make(chan struct{}) // Channel to signal graceful server shutdown

var currentChunk int64

// Define a middleware function for token authentication
func authenticate(validToken string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Extract the token from the Authorization header
		token := r.Header.Get("Authorization")
		token = strings.TrimPrefix(token, "Bearer ")

		// Check if the token is valid
		if token != validToken {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintln(w, "Unauthorized")
			return
		}

		// If the token is valid, call the next handler
		next.ServeHTTP(w, r)
	}
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprintf(w, "Method not allowed")
		return
	}

	chunkStr := r.URL.Query().Get("chunk")
	chunk, err := strconv.ParseInt(chunkStr, 10, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "Invalid chunk parameter: %v", err)
		return
	}

	// Read the raw request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "Error reading request body: %v", err)
		return
	}
	defer r.Body.Close()

	receivedSize := len(body)
	expectedSizeStr := r.URL.Query().Get("size")
	expectedSize, err := strconv.Atoi(expectedSizeStr)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "Invalid chunk parameter: %v", err)
		return
	}



	// Check if expected size is not equal to received size
	if receivedSize != expectedSize {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "Size parameter: received %d bytes, expected %d bytes in chunk %d\n", receivedSize, expectedSize, chunk)
		return
	}

	if chunk != atomic.LoadInt64(&currentChunk) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "Chunk order mismatch")
		return
	}

	// Set the current chunk to the received chunk
	atomic.StoreInt64(&currentChunk, chunk+1)

	// Print the raw body to stdout
	if _, err := os.Stdout.Write(body); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "Error writing request body to stdout: %v", err)
		return
	}

	// Respond to the client
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Request body received and printed to stdout\n")

	lastStr := r.URL.Query().Get("last")
	last, err := strconv.ParseBool(lastStr)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "Invalid last parameter: %v", err)
		return
	}
	if last {
		fmt.Fprintln(os.Stderr, "Completed: OK")
		stop <- struct{}{} // Signal to stop the server gracefully
		return
	}
}

func runServer(listenAddr string, authToken string, key string, crt string) {
	// Regular expression to match the format hostname_or_address:port
	addrRegex := regexp.MustCompile(`^([\w.-]+)?:\d+$`)
	if !addrRegex.MatchString(listenAddr) {
		fmt.Fprintln(os.Stderr, "Error: invalid address format. It should be in the format hostname_or_address:port")
		os.Exit(1)
	}
	http.HandleFunc("/upload", authenticate(authToken, uploadHandler))

	// Generate a self-signed certificate to use for HTTPS
	cert, err := tls.LoadX509KeyPair(crt, key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading certificate: %v\n", err)
		return
	}

	// Configure TLS
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}}

	// Start the HTTPS server
	server := &http.Server{
		Addr:      listenAddr,
		TLSConfig: tlsConfig,
	}

		go func() {
		<-stop // Wait for the stop signal
		fmt.Fprintln(os.Stderr, "\nShutting down server...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Error shutting down server: %v\n", err)
		}
	}()

	fmt.Fprintf(os.Stderr, "Server running on https://%s/upload\n", listenAddr)
	err = server.ListenAndServeTLS("", "")
	if err == http.ErrServerClosed {
		return
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error starting server: %v\n", err)
		return
	}

}

func runClient(url string, token string, maxRetries int, sleepFactor int, chunkSize int) {
	// Create a buffer for the chunk
	chunk := make([]byte, chunkSize)

	// Create HTTP client with custom TLS config to accept insecure connections
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // for self-signed certs
		},
	}

	chunkNumber := 0
	// Read binary data from stdin in chunks of 2MB
	for {
		var combinedChunk bytes.Buffer
		combinedSize := 0
		last := false

		// Read subsequent chunks to combine with the current chunk
		for combinedSize < chunkSize {
			n, err := os.Stdin.Read(chunk)
			if err != nil && err == io.EOF {
				last = true
			}
			if err != nil && err != io.EOF {
				fmt.Fprintf(os.Stderr, "Error reading from stdin: %v\n", err)
				os.Exit(1)
			}
			if n == 0 {
				break
			}
			combinedSize += n
			combinedChunk.Write(chunk[:n])
		}

		if combinedSize == 0 {
			break // No more data to read
		}

		req, err := http.NewRequest("POST", fmt.Sprintf("%s?chunk=%d&size=%d&last=%s", url, chunkNumber, combinedSize,strconv.FormatBool(last)), &combinedChunk)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating request: %v\n", err)
			os.Exit(1)
		}
		// Set authentication token
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/octet-stream")

		var resp *http.Response
		for retry := 0; retry < maxRetries; retry++ {
			resp, err = httpClient.Do(req)
			if err == nil && resp.StatusCode == http.StatusOK {
				break // Successful request
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error sending request: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "Failed to upload chunk %d. Server returned status: %d\n", chunkNumber, resp.StatusCode)
				response, err := io.ReadAll(resp.Body)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error reading server response: %v\n", err)
				} else {
					fmt.Fprintf(os.Stderr, "Server response: %s", response)
				}

			}

			// Apply exponential backoff before retrying
			sleepTime := time.Duration(sleepFactor<<retry) * time.Second
			fmt.Fprintf(os.Stderr, "Retrying in %s...\n", sleepTime)
			time.Sleep(sleepTime)
		}
		if err != nil || resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "Failed to upload chunk %d after %d retries. Exiting...\n", chunkNumber, maxRetries)
			os.Exit(1)
		}

		// Read and print the response body
		_, err = io.ReadAll(resp.Body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading server response: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf(".")
		chunkNumber++
	}
		fmt.Printf("\n")
}

func main() {
	var listenAddr string
	var connectUrl string
	var authToken string
	var maxRetries int
	var sleepFactor int
	var maxChunkSize int
	var key string
	var crt string
	flag.StringVar(&listenAddr, "listen", "", "Address and port to listen on")
	flag.StringVar(&listenAddr, "l", "", "Address and port to listen on (shorthand)")

	flag.StringVar(&connectUrl, "connect", "", "url for server to connect")
	flag.StringVar(&authToken, "token", "", "auth token")

	flag.StringVar(&key, "key", "key.pem", "tls key")
        flag.StringVar(&crt, "cert", "cert.pem", "tls crt")

	flag.IntVar(&maxRetries, "max-retries", 10, "Maximum retries with backoff time increase between retries, each retry affects a single chunk")
	flag.IntVar(&sleepFactor, "sleep-factor", 2, "sleep factor for backoff retries")
	flag.IntVar(&maxChunkSize, "chunk-size", 2*1024*1024, "transfer chunk size")
	flag.Parse()

	if listenAddr != "" {
		runServer(listenAddr, authToken, key, crt)
		os.Exit(0)
	}
	if connectUrl != "" {
		runClient(connectUrl, authToken, maxRetries, sleepFactor, maxChunkSize)
	}
}
