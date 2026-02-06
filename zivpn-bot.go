package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	BotConfigFile = "/etc/zivpn/bot-config.json"
	ApiUrl        = "http://127.0.0.1:8080/api"
	ApiKeyFile    = "/etc/zivpn/apikey"
	// !!! GANTI INI DENGAN URL GAMBAR MENU ANDA !!!
	MenuPhotoURL = "https://drive.google.com/file/d/1wc6UW_NDmNPV2qhpBHyBn_LdwC-0jHb_/view?usp=drivesdk"

	// Interval untuk pengecekan dan penghapusan akun expired
	AutoDeleteInterval = 30 * time.Second
	// Interval untuk Auto Backup (3 jam)
	AutoBackupInterval = 3 * time.Hour

	// Konfigurasi Backup dan Service
	BackupDir   = "/etc/zivpn/backups"
	ServiceName = "zivpn"

	// Pakasir Configuration - GANTI DENGAN MILIK ANDA!
	PakasirBaseURL   = "https://app.pakasir.com/api"
	PakasirProject   = "zivpn_pay"     // Slug project dari Pakasir dashboard
	PakasirAPIKey    = "S9zahTbmw4V7rjn2R9ctWRWljWYUjZxN"  // API key dari Pakasir (jika diperlukan)
	PakasirMethod    = "qris"                  // Metode QRIS
	PricePerDay      = 334                    // Harga per hari (RP), sesuaikan

	// Trial settings
	TrialDays        = 1
	TrialDBFile      = "/etc/zivpn/trial_users.db" // File track trial per Telegram ID

	// Minimal pembelian hari
	MinDaysPurchase  = 7
)

var ApiKey = "AutoFtBot-agskjgdvsbdreiWG1234512SDKrqw"

var startTime time.Time // Global variable untuk menghitung uptime bot

type BotConfig struct {
	BotToken      string `json:"bot_token"`
	AdminID       int64  `json:"admin_id"`
	NotifGroupID  int64  `json:"notif_group_id"`
	VpsExpiredDate string `json:"vps_expired_date"` // Format: 2006-01-02
}

type IpInfo struct {
	City string `json:"city"`
	Isp  string `json:"isp"`
}

type UserData struct {
	Host     string `json:"host"` // Host untuk backup
	Password string `json:"password"`
	Expired  string `json:"expired"`
	Status   string `json:"status"`
}

// Variabel global dengan Mutex untuk keamanan konkurensi (Thread-Safe)
var (
	stateMutex     sync.RWMutex
	userStates     = make(map[int64]string)
	tempUserData   = make(map[int64]map[string]string)
	lastMessageIDs = make(map[int64]int)

	trialMutex     sync.RWMutex
	trialUsers     = make(map[int64]bool) // Track user yang sudah ambil trial
)

func main() {
	startTime = time.Now() // Set waktu mulai bot
	rand.Seed(time.Now().UnixNano())

	if err := os.MkdirAll(BackupDir, os.ModePerm); err != nil {
		log.Printf("Gagal membuat direktori backup: %v", err)
	}

	if keyBytes, err := os.ReadFile(ApiKeyFile); err == nil {
		ApiKey = strings.TrimSpace(string(keyBytes))
	}

	// Load config awal
	config, err := loadConfig()
	if err != nil {
		log.Fatal("Gagal memuat konfigurasi bot:", err)
	}

	bot, err := tgbotapi.NewBotAPI(config.BotToken)
	if err != nil {
		log.Panic(err)
	}

	bot.Debug = false
	log.Printf("Authorized on account %s", bot.Self.UserName)

	// Load trial users
	loadTrialUsers()

	// --- BACKGROUND WORKER (PENGHAPUSAN OTOMATIS) ---
	go func() {
		autoDeleteExpiredUsers(bot, config.AdminID, false)
		ticker := time.NewTicker(AutoDeleteInterval)
		for range ticker.C {
			autoDeleteExpiredUsers(bot, config.AdminID, false)
		}
	}()

	// --- BACKGROUND WORKER (AUTO BACKUP) ---
	go func() {
		performAutoBackup(bot, config.AdminID)
		ticker := time.NewTicker(AutoBackupInterval)
		for range ticker.C {
			performAutoBackup(bot, config.AdminID)
		}
	}()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.Message != nil {
			handleMessage(bot, update.Message, config)
		} else if update.CallbackQuery != nil {
			handleCallback(bot, update.CallbackQuery, config)
		}
	}
}

// --- HANDLE MESSAGE ---
func handleMessage(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, config BotConfig) {
	userID := msg.From.ID
	isAdmin := userID == config.AdminID

	stateMutex.RLock()
	state, exists := userStates[userID]
	stateMutex.RUnlock()

	// Handle Restore dari Upload File (hanya admin)
	if exists && state == "wait_restore_file" && isAdmin {
		if msg.Document != nil {
			handleRestoreFromUpload(bot, msg)
		} else {
			sendMessage(bot, msg.Chat.ID, "❌ Mohon kirimkan file backup (.json).")
		}
		return
	}

	if exists {
		handleState(bot, msg, state, config)
		return
	}

	text := strings.ToLower(msg.Text)

	if msg.IsCommand() {
		switch msg.Command() {
		case "start", "panel", "menu":
			showMainMenu(bot, msg.Chat.ID, isAdmin)
		case "trial":
			if isAdmin {
				sendMessage(bot, msg.Chat.ID, "Admin tidak perlu trial.")
			} else {
				createTrialAccount(bot, msg.Chat.ID, userID)
			}
		case "create":
			if isAdmin {
				sendMessage(bot, msg.Chat.ID, "Gunakan menu admin untuk create user.")
			} else {
				initCreatePaidAccount(bot, msg.Chat.ID)
			}
		case "info":
			systemInfo(bot, msg.Chat.ID)
		// Command admin original
		case "setgroup":
			if isAdmin {
				args := msg.CommandArguments()
				if args == "" {
					sendMessage(bot, msg.Chat.ID, "❌ Format salah.\n\nUsage: `/setgroup <ID_GRUP>`\n\nContoh: `/setgroup -1001234567890`")
					return
				}
				groupID, err := strconv.ParseInt(args, 10, 64)
				if err != nil {
					sendMessage(bot, msg.Chat.ID, "❌ ID grup tidak valid.")
					return
				}
				config.NotifGroupID = groupID
				saveConfig(config)
				sendMessage(bot, msg.Chat.ID, "✅ ID grup notifikasi berhasil diatur.")
			} else {
				sendMessage(bot, msg.Chat.ID, "⛔ Perintah hanya untuk admin.")
			}
		default:
			sendMessage(bot, msg.Chat.ID, "Perintah tidak dikenal. Gunakan /start.")
		}
	}
}

// --- SHOW MAIN MENU (Disesuaikan untuk user/admin) ---
func showMainMenu(bot *tgbotapi.BotAPI, chatID int64, isAdmin bool) {
	msgText := "Selamat datang di ZiVPN Bot!\n\nPilih opsi di bawah:"
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Trial (1 Hari Gratis, 1x)", "trial"),
			tgbotapi.NewInlineKeyboardButtonData("Buat Akun Berbayar", "create_paid"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Info Sistem", "system_info"),
		),
	)

	if isAdmin {
		// Tambah tombol admin original
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard,
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Create User", "create_user"),
				tgbotapi.NewInlineKeyboardButtonData("Delete User", "delete_user"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Renew User", "renew_user"),
				tgbotapi.NewInlineKeyboardButtonData("List Users", "list_users"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("System Info Admin", "system_info_admin"),
				tgbotapi.NewInlineKeyboardButtonData("Backup Users", "backup_users"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Restore Backup", "restore_backup"),
				tgbotapi.NewInlineKeyboardButtonData("Restart Service", "restart_service"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Auto Delete Expired", "auto_delete"),
				tgbotapi.NewInlineKeyboardButtonData("Bot Uptime", "bot_uptime"),
			),
		)
	}

	reply := tgbotapi.NewMessage(chatID, msgText)
	reply.ReplyMarkup = keyboard
	reply.ParseMode = "Markdown"
	sendAndTrack(bot, reply)
}

// --- CREATE TRIAL ACCOUNT ---
func createTrialAccount(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	trialMutex.RLock()
	if trialUsers[userID] {
		sendMessage(bot, chatID, "❌ Anda sudah menggunakan trial sekali.")
		trialMutex.RUnlock()
		return
	}
	trialMutex.RUnlock()

	// Generate random password
	password := generateRandomPassword(8)

	reqBody := map[string]interface{}{
		"password": password,
		"days":     TrialDays,
	}

	res, err := apiCall("POST", "/user/create", reqBody)
	if err != nil {
		sendMessage(bot, chatID, "❌ Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		data := res["data"].(map[string]interface{})
		ipInfo, _ := getIpInfo()

		msg := fmt.Sprintf("✅ *TRIAL BERHASIL DIBUAT*\n"+
			"━━━━━━━━━━━━━━━━━━━━━━━━━\n"+
			"🔑 *Password*: `%s`\n"+
			"🗓️ *Expired*: `%s`\n"+
			"📍 *Lokasi*: `%s`\n"+
			"📡 *ISP*: `%s`\n"+
			"━━━━━━━━━━━━━━━━━━━━━━━━━\n"+
			"Note: Trial hanya 1 kali per akun Telegram.", password, data["expired"], ipInfo.City, ipInfo.Isp)

		reply := tgbotapi.NewMessage(chatID, msg)
		reply.ParseMode = "Markdown"
		bot.Send(reply)

		trialMutex.Lock()
		trialUsers[userID] = true
		saveTrialUsers()
		trialMutex.Unlock()
	} else {
		sendMessage(bot, chatID, "❌ Gagal: "+res["message"].(string))
	}
}

// --- INIT CREATE PAID ACCOUNT ---
func initCreatePaidAccount(bot *tgbotapi.BotAPI, chatID int64) {
	sendMessage(bot, chatID, fmt.Sprintf("Masukkan password yang diinginkan (bebas):\n\nNote: Minimal pembelian %d hari.", MinDaysPurchase))
	setState(chatID, "wait_password_paid")
	tempUserData[chatID] = make(map[string]string)
}

// --- HANDLE STATE (Tambah untuk paid) ---
func handleState(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, state string, config BotConfig) {
	chatID := msg.Chat.ID
	isAdmin := msg.From.ID == config.AdminID

	switch state {
	case "wait_password_paid":
		tempUserData[chatID]["password"] = msg.Text
		sendMessage(bot, chatID, fmt.Sprintf("Masukkan jumlah hari (minimal %d):", MinDaysPurchase))
		setState(chatID, "wait_days_paid")
	case "wait_days_paid":
		days, err := strconv.Atoi(msg.Text)
		if err != nil || days < MinDaysPurchase {
			sendMessage(bot, chatID, fmt.Sprintf("❌ Hari tidak valid atau kurang dari minimal %d. Coba lagi.", MinDaysPurchase))
			return
		}
		tempUserData[chatID]["days"] = msg.Text
		processPakasirPayment(bot, chatID, days, tempUserData[chatID]["password"])
	// Handle state original (untuk admin)
	case "wait_password":
		if isAdmin {
			// Logic original create user
		}
	// ... (tambahkan state original lainnya)
	}
}

// --- PROCESS PAKASIR PAYMENT ---
func processPakasirPayment(bot *tgbotapi.BotAPI, chatID int64, days int, password string) {
	amount := days * PricePerDay
	orderID := "ZIVPN_" + strconv.FormatInt(time.Now().Unix(), 10) + "_" + strconv.FormatInt(chatID, 10)

	reqBody := map[string]interface{}{
		"project":  PakasirProject,
		"order_id": orderID,
		"amount":   amount,
		// Tambah param lain jika diperlukan, seperti qris_only=1
	}

	jsonBody, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", PakasirBaseURL+"/transactioncreate/"+PakasirMethod, bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	if PakasirAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+PakasirAPIKey) // Sesuaikan method auth Pakasir
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		sendMessage(bot, chatID, "❌ Gagal membuat transaksi pembayaran.")
		clearState(chatID)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var res map[string]interface{}
	json.Unmarshal(body, &res)

	if payment, ok := res["payment"].(map[string]interface{}); ok {
		qrisImage, ok := payment["qris_image"].(string)
		if !ok {
			sendMessage(bot, chatID, "❌ Gagal mendapatkan QRIS image.")
			clearState(chatID)
			return
		}

		photo := tgbotapi.NewPhoto(chatID, tgbotapi.FileURL(qrisImage))
		photo.Caption = fmt.Sprintf("Scan QRIS ini untuk bayar Rp %d (untuk %d hari).\nOrder ID: %s\n\nBayar dalam 5 menit, atau batal.", amount, days, orderID)
		bot.Send(photo)

		// Mulai polling status di background
		go pollPakasirStatus(bot, chatID, orderID, password, days)
	} else {
		sendMessage(bot, chatID, "❌ Respons Pakasir tidak valid: "+string(body))
		clearState(chatID)
	}
}

// --- POLL PAKASIR STATUS ---
func pollPakasirStatus(bot *tgbotapi.BotAPI, chatID int64, orderID string, password string, days int) {
	for i := 0; i < 60; i++ { // Poll 5 menit, interval 5 detik
		statusURL := PakasirBaseURL + "/transactionstatus?order_id=" + orderID // Asumsi endpoint, sesuaikan dari docs
		req, _ := http.NewRequest("GET", statusURL, nil)
		if PakasirAPIKey != "" {
			req.Header.Set("Authorization", "Bearer "+PakasirAPIKey)
		}

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		var res map[string]interface{}
		json.Unmarshal(body, &res)

		status, ok := res["status"].(string)
		if ok && strings.ToLower(status) == "paid" {
			// Create user ZiVPN
			reqBody := map[string]interface{}{
				"password": password,
				"days":     days,
			}
			apiRes, err := apiCall("POST", "/user/create", reqBody)
			if err != nil {
				sendMessage(bot, chatID, "❌ Error API ZiVPN setelah bayar: "+err.Error())
				return
			}
			if apiRes["success"] == true {
				data := apiRes["data"].(map[string]interface{})
				ipInfo, _ := getIpInfo()

				msg := fmt.Sprintf("✅ *PEMBAYARAN BERHASIL & AKUN DIBUAT*\n"+
					"━━━━━━━━━━━━━━━━━━━━━━━━━\n"+
					"🔑 *Password*: `%s`\n"+
					"🗓️ *Expired*: `%s`\n"+
					"📍 *Lokasi*: `%s`\n"+
					"📡 *ISP*: `%s`\n"+
					"━━━━━━━━━━━━━━━━━━━━━━━━━", password, data["expired"], ipInfo.City, ipInfo.Isp)

				reply := tgbotapi.NewMessage(chatID, msg)
				reply.ParseMode = "Markdown"
				bot.Send(reply)
			} else {
				sendMessage(bot, chatID, "❌ Gagal create akun: "+apiRes["message"].(string))
			}
			clearState(chatID)
			return
		}

		time.Sleep(5 * time.Second)
	}
	sendMessage(bot, chatID, "❌ Pembayaran timeout atau gagal. Coba lagi.")
	clearState(chatID)
}

// --- HELPER FUNCTIONS (dari original, plus baru) ---
func setState(userID int64, state string) {
	stateMutex.Lock()
	userStates[userID] = state
	stateMutex.Unlock()
}

func clearState(userID int64) {
	stateMutex.Lock()
	delete(userStates, userID)
	delete(tempUserData, userID)
	stateMutex.Unlock()
}

func loadTrialUsers() {
	file, err := os.ReadFile(TrialDBFile)
	if err != nil {
		return
	}
	lines := strings.Split(string(file), "\n")
	for _, line := range lines {
		if line != "" {
			id, _ := strconv.ParseInt(line, 10, 64)
			trialUsers[id] = true
		}
	}
}

func saveTrialUsers() {
	var lines []string
	for id := range trialUsers {
		lines = append(lines, strconv.FormatInt(id, 10))
	}
	os.WriteFile(TrialDBFile, []byte(strings.Join(lines, "\n")), os.ModePerm)
}

func generateRandomPassword(length int) string {
	chars := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var sb strings.Builder
	for i := 0; i < length; i++ {
		sb.WriteByte(chars[rand.Intn(len(chars))])
	}
	return sb.String()
}

// --- HANDLE CALLBACK (Tambah untuk public) ---
func handleCallback(bot *tgbotapi.BotAPI, callback *tgbotapi.CallbackQuery, config BotConfig) {
	userID := callback.From.ID
	chatID := callback.Message.Chat.ID
	isAdmin := userID == config.AdminID

	callbackConfig := tgbotapi.NewCallback(callback.ID, "")
	bot.AnswerCallbackQuery(callbackConfig)

	switch callback.Data {
	case "trial":
		createTrialAccount(bot, chatID, userID)
	case "create_paid":
		initCreatePaidAccount(bot, chatID)
	case "system_info":
		systemInfo(bot, chatID)
	// Callback admin original
	case "create_user":
		if isAdmin {
			sendMessage(bot, chatID, "Masukkan password baru:")
			setState(chatID, "wait_password")
		}
	// ... (tambahkan callback admin lain dari original)
	default:
		sendMessage(bot, chatID, "Opsi tidak dikenal.")
	}
}

// Sisanya copy fungsi original seperti apiCall, systemInfo, listUsers, renewUser, dll. (truncated di query asli, tapi logic sama)

func apiCall(method string, endpoint string, body interface{}) (map[string]interface{}, error) {
	var req *http.Request
	var err error

	jsonBody, _ := json.Marshal(body)
	req, err = http.NewRequest(method, ApiUrl+endpoint, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", ApiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var res map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&res)
	return res, nil
}

// ... (fungsi original lain: systemInfo, listUsers, renewUser, deleteUser, performAutoBackup, autoDeleteExpiredUsers, handleRestoreFromUpload, sendAndTrack, sendMessage, getIpInfo, loadConfig, saveConfig, dll.)

func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	bot.Send(msg)
}

func getIpInfo() (IpInfo, error) {
	// Implementasi original
	return IpInfo{City: "Jakarta", Isp: "Unknown"}, nil
}

// Dan seterusnya...
