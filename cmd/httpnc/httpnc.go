package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ClientData struct {
	url string
	buf []byte
	last bool
}

type ChunkData struct {
	id int64
	buf []byte
	last bool
}



var (
	lastChunk      int64 = -1
	chunks         map[int64]int
	mutex          sync.Mutex
	notificationCh chan int64
	stop           = make(chan struct{}, 2) // Channel to signal graceful server shutdown
	clientBuffers    []ClientData    // Array of structs to hold client data
	serverBuffers    []ChunkData
	freeClients      chan int        // Global buffered channel with maxClients depth
	clientTasks      chan int        // Channel for client tasks
	wg             sync.WaitGroup
)

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

	expectedSizeStr := r.URL.Query().Get("size")
	expectedSize, err := strconv.Atoi(expectedSizeStr)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "Invalid chunk parameter: %v", err)
		return
	}

	chunkBufferId := <- freeClients
	receivedSize := 0
	if expectedSize != 0 {
		bytesRead, err := io.ReadFull(r.Body, serverBuffers[chunkBufferId].buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "Error reading request body: %v", err)
			return
		}
		receivedSize = bytesRead
	}
	serverBuffers[chunkBufferId].buf = serverBuffers[chunkBufferId].buf[:receivedSize]
	defer r.Body.Close()



	// Check if expected size is not equal to received size
	if receivedSize != expectedSize {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "Size parameter: received %d bytes, expected %d bytes in chunk %d\n", receivedSize, expectedSize, chunk)
		return
	}

	lastStr := r.URL.Query().Get("last")
	last, err := strconv.ParseBool(lastStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid last parameter: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "Invalid last parameter: %v", err)
		return
	}

	// Respond to the client
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Request body received\n")

	go func() {
		wg.Add(1)
		mutex.Lock()
		if last {
			atomic.StoreInt64(&lastChunk, chunk)
		}

		if chunks == nil {
			chunks = make(map[int64]int)
		}
		chunks[chunk] = chunkBufferId
		mutex.Unlock()
		notificationCh <- chunk
		wg.Done()
	} ()
}

func chunkProcessor() {
	cur_seqno := int64(0)
	for {
		_ = <-notificationCh

		for {
			select {
			case <-notificationCh:
			default:
			}

			found_next := false
			chunkBufferId := -1

			mutex.Lock()
			if val, ok := chunks[cur_seqno]; ok {
				found_next = true
				chunkBufferId = val // Remove the item from the map
				delete(chunks, cur_seqno)
			}
			mutex.Unlock()

			if !found_next {
				break
			}

			// Print the raw body to stdout
			if _, err := os.Stdout.Write(serverBuffers[chunkBufferId].buf); err != nil {
				fmt.Fprintf(os.Stderr, "Error writing request body to stdout: %v\n", err)
				os.Exit(2)
			}

			if cur_seqno == atomic.LoadInt64(&lastChunk) {
				fmt.Fprintln(os.Stderr, "Completed: OK")
				wg.Wait()
				stop <- struct{}{} // Signal to stop the server gracefully
				return
			}
			cur_seqno++
			freeClients <- chunkBufferId
		}
	}
}

func runServer(listenAddr string, authToken string, key string, crt string, parallelClients int, chunkSize int) {
	notificationCh = make(chan int64, parallelClients)
	serverBuffers = make([]ChunkData, parallelClients)

	for i := 0; i < parallelClients; i++ {
		serverBuffers[i].buf = make([]byte, chunkSize)
	}
	freeClients = make(chan int, parallelClients)

	for i := 0; i < parallelClients; i++ {
		freeClients <- i
	}


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
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Error shutting down server: %v\n", err)
		}
	}()

	go chunkProcessor()

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

func sendChunk(token string, maxRetries int, sleepFactor int, connectionClose bool, insecureSkipVerify bool, CAFile string) {
	defer wg.Done()

	tlsConfig := &tls.Config{InsecureSkipVerify: insecureSkipVerify}
	if CAFile != "" {
		// Load CA certificate
		caCert, err := os.ReadFile(CAFile)
		if err != nil {
			fmt.Println("Error reading CA certificate file:", err)
			return
		}

		// Create a certificate pool and add CA certificate
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)

		// Create a TLS configuration with custom CA certificate pool
		tlsConfig = &tls.Config{
			RootCAs: caCertPool,
			InsecureSkipVerify: insecureSkipVerify,
		}
	}
	// Create HTTP client with custom TLS config to accept insecure connections
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig, // for self-signed certs
		},
	}

	for {
		index := 0
		select {
		case index = <-clientTasks:
		case <-stop:
			stop <- struct{}{} 
			return
		}

		req, err := http.NewRequest("POST", clientBuffers[index].url, bytes.NewReader(clientBuffers[index].buf))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating request: %v\n", err)
			os.Exit(1)
		}
		// Set authentication token
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Close = connectionClose

		var resp *http.Response
		for retry := 0; retry < maxRetries; retry++ {
			resp, err = httpClient.Do(req)
			if err == nil && resp.StatusCode == http.StatusOK {
				break // Successful request
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error sending request: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "Failed to upload chunk %s. Server returned status: %d\n", clientBuffers[index].url, resp.StatusCode)
				response, err := io.ReadAll(resp.Body)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error reading server response: %v\n", err)
				} else {
					fmt.Fprintf(os.Stderr, "Server response: %s", response)
				}
				resp.Body.Close()
			}

			// Apply exponential backoff before retrying
			sleepTime := time.Duration(sleepFactor<<retry) * time.Second
			fmt.Fprintf(os.Stderr, "Retrying in %s...\n", sleepTime)
			time.Sleep(sleepTime)
		}

		defer resp.Body.Close()


		if err != nil || resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "Failed to upload chunk %s after %d retries. Exiting...\n", clientBuffers[index].url, maxRetries)
			os.Exit(1)
		}

		_, err = io.ReadAll(resp.Body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading server response: %v\n", err)
		}


		if clientBuffers[index].last {
			stop <- struct{}{} 
			return
		}

		// Signal that processing is done and the index is available
		freeClients <- index
	}
}

func runClient(url string, token string, maxRetries int, sleepFactor int, chunkSize int, maxClients int, insecureSkipVerify bool, CAFile string) {
	chunkNumber := 0

	clientBuffers = make([]ClientData, maxClients)

	// Initialize freeClients channel
	freeClients = make(chan int, maxClients)

	// Populate freeClients with client indices
	for i := 0; i < maxClients; i++ {
		freeClients <- i
		wg.Add(1)
		go sendChunk(token, maxRetries, sleepFactor, false, insecureSkipVerify, CAFile)
	}

	// Initialize clientTasks channel
	clientTasks = make(chan int)

	for i := 0; i < maxClients; i++ {
		clientBuffers[i].buf = make([]byte, chunkSize)
	}
	// Read binary data from stdin in chunks of 2MB
	for {
		last := false
		clientId := <-freeClients
		bytesRead, err := io.ReadFull(os.Stdin, clientBuffers[clientId].buf)
		if err != nil && err == io.EOF {
			last = true
		}

		clientBuffers[clientId].last = last

		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			fmt.Fprintf(os.Stderr, "Error reading from stdin: %v\n", err)
			break
		}

		// Adjust the length of the buffer according to the number of bytes read
		clientBuffers[clientId].buf = clientBuffers[clientId].buf[:bytesRead]
		clientBuffers[clientId].url = fmt.Sprintf(
			"%s?chunk=%d&size=%d&last=%s",
			url,
			chunkNumber,
			len(clientBuffers[clientId].buf),
			strconv.FormatBool(last))
		chunkNumber++


		// Signal that buffer is filled and ready for processing
		clientTasks <- clientId

		if last {
			break // No more data to read
		}
	}

	// Wait for all tasks to finish
	wg.Wait()
}

func main() {
	var listenAddr string
	var connectUrl string
	var authToken string
	var maxRetries int
	var sleepFactor int
	var maxChunkSize int
	var parallelClients int
	var key string
	var crt string
	var ca_file string
	var insecure bool
	flag.StringVar(&listenAddr, "listen", "", "Address and port to listen on")
	flag.StringVar(&listenAddr, "l", "", "Address and port to listen on (shorthand)")

	flag.StringVar(&connectUrl, "connect", "", "url for server to connect")
	flag.StringVar(&authToken, "token", "", "auth token")

	flag.StringVar(&key, "key", "key.pem", "tls key")
	flag.StringVar(&crt, "cert", "cert.pem", "tls crt")
	flag.StringVar(&ca_file, "ca", "", "server's CA certificate")
	flag.BoolVar(&insecure, "insecure", true, "use false to enable tls checks")

	flag.IntVar(&maxRetries, "max-retries", 10, "Maximum retries with backoff time increase between retries, each retry affects a single chunk")
	flag.IntVar(&sleepFactor, "sleep-factor", 2, "sleep factor for backoff retries")
	flag.IntVar(&maxChunkSize, "chunk-size", 2*1024*1024, "transfer chunk size")
	flag.IntVar(&parallelClients, "parallel", 16, "maximum clients")
	flag.Parse()

	if listenAddr != "" {
		runServer(listenAddr, authToken, key, crt, parallelClients, maxChunkSize)
		os.Exit(0)
	}
	if connectUrl != "" {
		runClient(connectUrl, authToken, maxRetries, sleepFactor, maxChunkSize, parallelClients, insecure, ca_file)
	}
}
