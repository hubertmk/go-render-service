package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hschendel/stl"
	"github.com/fogleman/fauxgl"
)

const (
	Width      = 1024
	Height     = 1024
	FOV        = 30
	HashesFile = "file_hashes.json" // JSON file to store processed file hashes
)

var (
	queue          = make(chan Job, 100)              // Channel to queue jobs for STL processing
	upgrader       = websocket.Upgrader{}
	tmpl           = template.Must(template.ParseFiles("templates/index.html"))
	mu             sync.Mutex
	jobConnections = make(map[int64]*websocket.Conn)   // Track WebSocket connections by Job ID
	fileHashes     = make(map[string]string)           // Track file hashes and their output paths
)

type Job struct {
	ID         int64
	STLPath    string
	OutputPath string
}

func main() {
	// Load file hashes from JSON on startup
	if err := loadFileHashes(); err != nil {
		log.Printf("Error loading file hashes: %v", err)
	}

	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/upload", uploadHandler)
	http.HandleFunc("/ws", wsHandler)
	go processQueue()

	// Static file server for PNG output and other static assets
	http.Handle("/output/", http.StripPrefix("/output/", http.FileServer(http.Dir("output"))))

	log.Println("Server started at http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// Helper Functions

// Load saved file hashes from JSON on startup
func loadFileHashes() error {
	data, err := ioutil.ReadFile(HashesFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No hashes file yet, skip loading
		}
		return err
	}
	return json.Unmarshal(data, &fileHashes)
}

// Save the current file hashes to JSON
func saveFileHashes() error {
	data, err := json.Marshal(fileHashes)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(HashesFile, data, 0644)
}

// Template handler
func indexHandler(w http.ResponseWriter, r *http.Request) {
	if err := tmpl.Execute(w, nil); err != nil {
		http.Error(w, "Could not load template", http.StatusInternalServerError)
		log.Printf("Template execution error: %v", err)
	}
}

// Check if a file already exists based on its hash
func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
		return
	}

	// Parse uploaded file
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Failed to read file", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	// Calculate the SHA-256 hash of the file content
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		http.Error(w, "Failed to calculate file hash", http.StatusInternalServerError)
		return
	}
	fileHash := hex.EncodeToString(hash.Sum(nil))

	// Check if this file hash already exists
	mu.Lock()
	outputFileName, exists := fileHashes[fileHash]
	mu.Unlock()

	if exists {
		// File has already been processed, no need to reprocess
		downloadLink := fmt.Sprintf("/output/%s", filepath.Base(outputFileName))
		fmt.Fprintf(w, "This file has already been processed. <a href='%s'>Download the existing output here</a>", downloadLink)
		return
	}

	// Save the file to a unique path in the uploads folder
	stlPath := filepath.Join("uploads", fmt.Sprintf("input-%s.stl", fileHash))
	outputFileName = fmt.Sprintf("output-%s.png", fileHash)

	// Save the uploaded file
	file.Seek(0, io.SeekStart)
	fileBytes, err := ioutil.ReadAll(file)
	if err != nil {
		http.Error(w, "Failed to read file content", http.StatusInternalServerError)
		return
	}
	err = ioutil.WriteFile(stlPath, fileBytes, 0644)
	if err != nil {
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}

	// Delay job queuing until the WebSocket connection is established
	fmt.Fprintf(w, "%d|%s|%s", time.Now().Unix(), stlPath, outputFileName) // Send job details to client
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade failed:", err)
		return
	}
	defer conn.Close()

	// Read the job ID and paths from the WebSocket message
	_, jobDetailsBytes, err := conn.ReadMessage()
	if err != nil {
		log.Println("Failed to read job details:", err)
		return
	}
	details := string(jobDetailsBytes)
	parts := strings.Split(details, "|")

	// Ensure the message has at least 3 parts (jobID, stlPath, outputPath)
	if len(parts) < 3 {
		log.Println("Received invalid job details format:", details)
		return
	}

	jobID, _ := strconv.ParseInt(parts[0], 10, 64)
	stlPath, outputPath := parts[1], parts[2]

	// Register the WebSocket connection for the job ID
	mu.Lock()
	jobConnections[jobID] = conn
	mu.Unlock()

	log.Printf("WebSocket connection established for job ID: %d\n", jobID)

	// Queue the job for processing
	queue <- Job{ID: jobID, STLPath: stlPath, OutputPath: outputPath}

	// Keep connection open until manually closed
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}

	// If connection closes, log and remove from connections
	mu.Lock()
	delete(jobConnections, jobID)
	mu.Unlock()
	log.Printf("WebSocket connection closed for job ID: %d\n", jobID)
}


func processQueue() {
	for job := range queue {
		log.Printf("Processing job ID: %d\n", job.ID)

		// Short delay to ensure WebSocket connection is established
		time.Sleep(100 * time.Millisecond)
		notifyClient(job.ID, "Processing your file...")

		// Render the STL to PNG
		outputPath, err := renderSTLToPNG(job)
		if err != nil {
			log.Println("Failed to render STL:", err)
			notifyClient(job.ID, "Failed to render file. Please try again.")
			continue
		}

		// Store the file hash only after successful processing
		fileHash := strings.TrimPrefix(filepath.Base(job.STLPath), "input-")
		fileHash = strings.TrimSuffix(fileHash, ".stl")

		mu.Lock()
		fileHashes[fileHash] = filepath.Base(outputPath)
		saveFileHashes()
		mu.Unlock()

		// Send the rendering complete message with download link
		downloadLink := fmt.Sprintf("/output/%s", filepath.Base(outputPath))
		notifyClient(job.ID, fmt.Sprintf("Rendering complete! <a href='%s'>Download your image here</a>", downloadLink))
		log.Printf("Completed job ID: %d\n", job.ID)
	}
}


func notifyClient(jobID int64, message string) {
	mu.Lock()
	conn, ok := jobConnections[jobID]
	mu.Unlock()

	if !ok {
		log.Printf("No WebSocket connection found for job ID: %d\n", jobID)
		return
	}

	err := conn.WriteMessage(websocket.TextMessage, []byte(message))
	if err != nil {
		log.Printf("Failed to send message to job ID %d: %v\n", jobID, err)

		// Close the WebSocket connection if it's no longer active
		conn.Close()

		mu.Lock()
		delete(jobConnections, jobID)
		mu.Unlock()
	} else {
		log.Printf("Successfully sent message to job ID %d: %s\n", jobID, message)
	}
}


// Render STL to PNG using fauxgl
func renderSTLToPNG(job Job) (string, error) {
	reader, err := stl.ReadFile(job.STLPath)
	if err != nil {
		return "", fmt.Errorf("failed to read STL file: %w", err)
	}

	mesh := fauxgl.NewEmptyMesh()
	for _, triangle := range reader.Triangles {
		v1 := fauxgl.Vector{float64(triangle.Vertices[0][0]), float64(triangle.Vertices[0][1]), float64(triangle.Vertices[0][2])}
		v2 := fauxgl.Vector{float64(triangle.Vertices[1][0]), float64(triangle.Vertices[1][1]), float64(triangle.Vertices[1][2])}
		v3 := fauxgl.Vector{float64(triangle.Vertices[2][0]), float64(triangle.Vertices[2][1]), float64(triangle.Vertices[2][2])}
		mesh.Triangles = append(mesh.Triangles, fauxgl.NewTriangleForPoints(v1, v2, v3))
	}
	mesh.BiUnitCube()

	context := fauxgl.NewContext(Width, Height)
	context.ClearColorBufferWith(fauxgl.HexColor("#ffffff"))

	eye := fauxgl.Vector{3, 3, 3}
	center := fauxgl.Vector{0, 0, 0}
	up := fauxgl.Vector{0, 0, 1}
	matrix := fauxgl.LookAt(eye, center, up).Perspective(FOV, float64(Width)/float64(Height), 1, 10)
	light := fauxgl.Vector{1, 1, 1}.Normalize()
	shader := fauxgl.NewPhongShader(matrix, light, eye)
	shader.ObjectColor = fauxgl.Gray(0.75)
	shader.SpecularPower = 100
	context.Shader = shader
	context.DrawMesh(mesh)

	outputPath := filepath.Join("output", job.OutputPath)
	err = fauxgl.SavePNG(outputPath, context.Image())
	if err != nil {
		return "", fmt.Errorf("failed to save PNG file: %w", err)
	}

	return outputPath, nil
}

