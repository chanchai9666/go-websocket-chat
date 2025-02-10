package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	_ "github.com/mattn/go-sqlite3"
)

var (
	db        *sql.DB
	clients   sync.Map                   // แทนที่ map + mutex
	broadcast = make(chan Message, 5000) // เพิ่ม buffer เพื่อรองรับโหลดสูง
)

// โครงสร้างข้อความ
type Message struct {
	ID         int64  `json:"id"`
	SenderID   string `json:"sender_id"`
	ReceiverID string `json:"receiver_id"`
	Text       string `json:"text"`
	IsRead     bool   `json:"is_read"`
}

func initDB() {
	var err error

	// ใช้ SQLite
	databaseURL := "file:chat.db?cache=shared&mode=rwc" // SQLite default
	// สร้างการเชื่อมต่อ
	db, err = sql.Open("sqlite3", databaseURL)
	if err != nil {
		log.Fatalf("Database connection error: %v", err)
	}

	// ตรวจสอบการเชื่อมต่อผ่าน ping
	err = db.Ping()
	if err != nil {
		log.Fatalf("Failed to ping database: %v", err)
	}

	// ตั้งค่าฐานข้อมูลเพื่อให้รองรับการทำงานพร้อมกันได้ดีขึ้น
	_, err = db.Exec("PRAGMA journal_mode=WAL;")
	if err != nil {
		log.Fatalf("Error setting PRAGMA journal_mode: %v", err)
	}

	db.SetMaxOpenConns(50)                 // เปิดได้สูงสุด 50 connections
	db.SetMaxIdleConns(25)                 // ค้าง connection ไว้ใน pool
	db.SetConnMaxLifetime(5 * time.Minute) // อายุสูงสุด 5 นาที

	// สร้างตารางหากยังไม่มี
	createTable()

	fmt.Println("Connected to SQLite successfully")
}

func createTable() {
	// สร้างตารางสำหรับบันทึกข้อความ
	query := `
	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		sender_id TEXT,
		receiver_id TEXT,
		text TEXT,
		is_read BOOLEAN DEFAULT FALSE
	);`
	_, err := db.Exec(query)
	if err != nil {
		log.Fatalf("Error creating table: %v", err)
	}
}

func main() {
	initDB()

	app := fiber.New()
	app.Get("/chat", func(c *fiber.Ctx) error {
		return c.SendFile("./index.html")
	})
	// Route สำหรับ WebSocket
	app.Get("/ws/chat/:id", websocket.New(handleWebSocket))

	// Route สำหรับดึงรายชื่อผู้ใช้งานออนไลน์
	app.Get("/online", func(c *fiber.Ctx) error {
		onlineUsers := getOnlineUsers()
		return c.JSON(fiber.Map{
			"online_users": onlineUsers,
			"count":        len(onlineUsers),
		})
	})

	// API รับข้อความโดยไม่ต้อง Connect WebSocket
	app.Post("/send", func(c *fiber.Ctx) error {
		var msg Message
		if err := c.BodyParser(&msg); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "Invalid request"})
		}

		// เช็กว่าผู้รับออนไลน์หรือไม่
		if conn, exists := clients.Load(msg.ReceiverID); exists {
			response, _ := json.Marshal(msg)
			conn.(*websocket.Conn).WriteMessage(websocket.TextMessage, response)

			fmt.Printf("[SEND] %s -> %s: %s (Online)\n", msg.SenderID, msg.ReceiverID, msg.Text)
		} else {
			// ถ้าออฟไลน์ เก็บลง DB
			saveMessageToDB(msg)
			fmt.Printf("[SAVE] %s -> %s: %s (Offline, saved to DB)\n", msg.SenderID, msg.ReceiverID, msg.Text)
		}

		return c.JSON(fiber.Map{"status": "Message processed"})
	})

	// เปิด Worker Pool สำหรับจัดการข้อความ (50 Worker คือจำนวนข้อความที่จะส่งพร้อมกัน)
	for i := 0; i < 50; i++ { // 50 Worker
		go messageWorker()
	}

	log.Fatal(app.Listen(":3000"))
}

func handleWebSocket(c *websocket.Conn) {
	clientID := c.Params("id")
	clients.Store(clientID, c) // ✅ เก็บ WebSocket Conn ของผู้ใช้

	// ✅ Log ตอน Connect
	fmt.Printf("[CONNECT] User %s connected\n", clientID)

	// ส่งข้อความที่ค้างไว้
	sendPendingMessages(clientID, c)

	defer func() {
		clients.Delete(clientID)
		c.Close()
		// ✅ Log ตอน Disconnect
		fmt.Printf("[DISCONNECT] User %s disconnected\n", clientID)
	}()

	for {
		_, msg, err := c.ReadMessage()
		if err != nil {
			break
		}

		var receivedMsg Message
		if err := json.Unmarshal(msg, &receivedMsg); err != nil {
			continue
		}

		// ✅ Log ตอนส่งข้อความจาก Client
		fmt.Printf("[MESSAGE] %s -> %s: %s\n", receivedMsg.SenderID, receivedMsg.ReceiverID, receivedMsg.Text)

		broadcast <- receivedMsg
	}
}

// Worker Pool สำหรับจัดการข้อความ
func messageWorker() {
	for msg := range broadcast {
		// ตรวจสอบว่า ReceiverID เชื่อมต่ออยู่หรือไม่
		if conn, exists := clients.Load(msg.ReceiverID); exists {
			wsConn := conn.(*websocket.Conn)
			response, err := json.Marshal(msg)
			if err != nil {
				log.Printf("Error marshalling message: %v\n", err)
				continue
			}

			// Log ส่งข้อความให้ผู้รับออนไลน์
			fmt.Printf("[SEND] %s -> %s: %s (Online)\n", msg.SenderID, msg.ReceiverID, msg.Text)

			// พยายามส่งข้อความผ่าน WebSocket
			if err := wsConn.WriteMessage(websocket.TextMessage, response); err != nil {
				log.Printf("Error sending message to user %s: %v\n", msg.ReceiverID, err)
				// ถ้าเกิดข้อผิดพลาดในการส่ง, ลบการเชื่อมต่อและบันทึกข้อความลง DB
				clients.Delete(msg.ReceiverID)
				saveMessageToDB(msg)
			}
		} else {
			// ผู้รับออฟไลน์ (ไม่มีการเชื่อมต่อ WebSocket)
			// Log ตอนบันทึกข้อความลงฐานข้อมูล
			fmt.Printf("[SAVE] %s -> %s: %s (Offline, saved to DB)\n", msg.SenderID, msg.ReceiverID, msg.Text)
			saveMessageToDB(msg)
		}
	}
}

// ฟังก์ชันบันทึกข้อความลงฐานข้อมูล
func saveMessageToDB(msg Message) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("Error starting transaction: %v\n", err)
		return
	}

	stmt, err := tx.Prepare("INSERT INTO messages (sender_id, receiver_id, text) VALUES (?, ?, ?)")
	if err != nil {
		log.Printf("Error preparing statement: %v\n", err)
		tx.Rollback()
		return
	}
	defer stmt.Close()

	_, err = stmt.Exec(msg.SenderID, msg.ReceiverID, msg.Text)
	if err != nil {
		log.Printf("Error executing insert: %v\n", err)
		tx.Rollback()
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Error committing transaction: %v\n", err)
	}
}

// ส่งข้อความที่ค้างไว้ให้ผู้ใช้ที่พึ่งเชื่อมต่อ
func sendPendingMessages(userID string, conn *websocket.Conn) {
	rows, err := db.Query("SELECT id, sender_id, receiver_id, text FROM messages WHERE receiver_id = ? AND is_read = FALSE", userID)
	if err != nil {
		log.Println("Error fetching messages:", err)
		return
	}
	defer rows.Close()

	var msg Message
	var msgUpdate []interface{}
	for rows.Next() {
		if err := rows.Scan(&msg.ID, &msg.SenderID, &msg.ReceiverID, &msg.Text); err != nil {
			log.Println("Error scanning message:", err)
			continue
		}

		// ส่งข้อความให้ WebSocket
		response, _ := json.Marshal(msg)
		if err := conn.WriteMessage(websocket.TextMessage, response); err == nil {
			msgUpdate = append(msgUpdate, msg.ID)
		}
	}

	// อัปเดตสถานะข้อความ
	if len(msgUpdate) > 0 {
		// ใช้ strings.Join เพื่อสร้างคำสั่ง IN สำหรับ SQL
		query := fmt.Sprintf("UPDATE messages SET is_read = TRUE WHERE id IN (%s)", strings.Join(makePlaceholders(len(msgUpdate)), ","))
		// แสดงคำสั่ง SQL ที่จะถูก execute
		log.Printf("Executing SQL: %s\n", query)

		_, err := db.Exec(query, msgUpdate...)
		if err != nil {
			log.Println("Error updating message status:", err)
		}
	}
}

// คืนค่าผู้ใช้ที่ออนไลน์
func getOnlineUsers() []string {
	onlineUsers := make([]string, 0)

	clients.Range(func(key, value any) bool {
		onlineUsers = append(onlineUsers, key.(string))
		return true
	})

	return onlineUsers
}

// ฟังก์ชันที่สร้าง placeholders สำหรับคำสั่ง SQL
func makePlaceholders(n int) []string {
	placeholders := make([]string, n)
	for i := range placeholders {
		placeholders[i] = "?"
	}
	return placeholders
}
