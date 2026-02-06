package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
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
	MenuPhotoURL  = "https://i.ibb.co/0jZ0Z0Z/zivpn-menu.jpg" // Ganti dengan link public Anda (imgbb/imghippo)

	AutoDeleteInterval = 30 * time.Second
	AutoBackupInterval = 3 * time.Hour
	BackupDir          = "/etc/zivpn/backups"
	ServiceName        = "zivpn"

	// Pakasir Configuration - GANTI DENGAN MILIK ANDA!
	PakasirBaseURL   = "https://app.pakasir.com/api"
	PakasirProject   = "your_project_slug"     // Slug project dari Pakasir dashboard
	PakasirAPIKey    = "your_pakasir_api_key"  // API key dari Pakasir (jika diperlukan)
	PakasirMethod    = "qris"                  // Metode QRIS
	PricePerDay      = 1000                    // Harga per hari (RP), sesuaikan

	// Trial settings
	TrialDays        = 1
	TrialDBFile      = "/etc/zivpn/trial_users.db" // File track trial per Telegram ID

	// Minimal pembelian hari
	MinDaysPurchase  = 7
)

var (
	ApiKey     string
	startTime  = time.Now()
	stateMutex sync.RWMutex
	userStates = make(map[int64]string)
	tempData   = make(map[int64]map[string]string)
	lastMsgID  = make(map[int64]int)
	trialUsers = make(map[int64]bool)
	trialMutex sync.RWMutex
)

type BotConfig struct {
	BotToken string `json:"bot_token"`
	AdminID  int64  `json:"admin_id"`
}

type IpInfo struct {
	City string `json:"city"`
	Isp  string `json:"isp"`
}

func main() {
	// Load API Key dari file (bukan hardcoded)
	if b, err := ioutil.ReadFile(ApiKeyFile); err == nil {
		ApiKey = strings.TrimSpace(string(b))
	}

	config, err := loadConfig()
	if err != nil {
		log.Fatal("Gagal memuat bot-config.json: ", err)
	}

	bot, err := tgbotapi.NewBotAPI(config.BotToken)
	if err != nil {
		log.Panic("Token salah atau internet bermasalah: ", err)
	}

	log.Printf("Bot started: @%s", bot.Self.UserName)

	loadTrialUsers()

	// Background tasks
	go func() {
		autoDeleteExpiredUsers(bot, config.AdminID)
		ticker := time.NewTicker(AutoDeleteInterval)
		for range ticker.C {
			autoDeleteExpiredUsers(bot, config.AdminID)
		}
	}()

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
				// ... (kode setgroup dari original)
			} else {
				sendMessage(bot, msg.Chat.ID, "⛔ Perintah hanya untuk admin.")
			}
		default:
			sendMessage(bot, msg.Chat.ID, "Perintah tidak dikenal. Gunakan /start.")
		}
	}
}

// --- SHOW MAIN MENU ---
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
		// Tambah tombol admin original dari repo Anda
		// ... (append keyboard untuk admin buttons)
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

	password := generateRandomPassword(8)

	reqBody := map[string]interface{}{
		"password": password,
		"days":     TrialDays,
	}

	res, err := apiCall("POST", "/user/create", reqBody)
	if err != nil {
		sendMessage(bot, chatID, "❌ Error API: " + err.Error())
		return
	}

	if res["success"].(bool) {
		data := res["data"].(map[string]interface{})
		ipInfo, _ := getIpInfo()

		msg := fmt.Sprintf("✅ *TRIAL BERHASIL DIBUAT*\n" +
			"━━━━━━━━━━━━━━━━━━━━━━━━━\n" +
			"🔑 *Password*: `%s`\n" +
			"🗓️ *Expired*: `%s`\n" +
			"📍 *Lokasi*: `%s`\n" +
			"📡 *ISP*: `%s`\n" +
			"━━━━━━━━━━━━━━━━━━━━━━━━━\n" +
			"Note: Trial hanya 1 kali per akun Telegram.", password, data["expired"], ipInfo.City, ipInfo.Isp)

		reply := tgbotapi.NewMessage(chatID, msg)
		reply.ParseMode = "Markdown"
		bot.Send(reply)

		trialMutex.Lock()
		trialUsers[userID] = true
		saveTrialUsers()
		trialMutex.Unlock()
	} else {
		sendMessage(bot, chatID, "❌ Gagal: " + res["message"].(string))
	}
}

// --- INIT CREATE PAID ACCOUNT ---
func initCreatePaidAccount(bot *tgbotapi.BotAPI, chatID int64) {
	sendMessage(bot, chatID, fmt.Sprintf("Masukkan password yang diinginkan (bebas):\n\nNote: Minimal pembelian %d hari.", MinDaysPurchase))
	setState(chatID, "wait_password_paid")
	tempData[chatID] = make(map[string]string)
}

// --- HANDLE STATE ---
func handleState(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, state string, config BotConfig) {
	chatID := msg.Chat.ID

	switch state {
	case "wait_password_paid":
		tempData[chatID]["password"] = msg.Text
		sendMessage(bot, chatID, fmt.Sprintf("Masukkan jumlah hari (minimal %d):", MinDaysPurchase))
		setState(chatID, "wait_days_paid")
	case "wait_days_paid":
		days, err := strconv.Atoi(msg.Text)
		if err != nil || days < MinDaysPurchase {
			sendMessage(bot, chatID, fmt.Sprintf("❌ Hari tidak valid atau kurang dari minimal %d. Coba lagi.", MinDaysPurchase))
			return
		}
		tempData[chatID]["days"] = msg.Text
		processPakasirPayment(bot, chatID, days, tempData[chatID]["password"])
	// Tambah state admin dari original jika ada
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
	}

	jsonBody, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", PakasirBaseURL+"/transactioncreate/"+PakasirMethod, bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	if PakasirAPIKey != "" {
		req.Header.Set("Authorization", "Bearer " + PakasirAPIKey)
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

		go pollPakasirStatus(bot, chatID, orderID, password, days)
	} else {
		sendMessage(bot, chatID, "❌ Respons Pakasir tidak valid: " + string(body))
		clearState(chatID)
	}
}

// --- POLL PAKASIR STATUS ---
func pollPakasirStatus(bot *tgbotapi.BotAPI, chatID int64, orderID string, password string, days int) {
	for i := 0; i < 60; i++ { // Poll 5 menit, interval 5 detik
		statusURL := PakasirBaseURL + "/transactionstatus?order_id=" + orderID
		req, _ := http.NewRequest("GET", statusURL, nil)
		if PakasirAPIKey != "" {
			req.Header.Set("Authorization", "Bearer " + PakasirAPIKey)
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
			reqBody := map[string]interface{}{
				"password": password,
				"days":     days,
			}
			apiRes, err := apiCall("POST", "/user/create", reqBody)
			if err != nil {
				sendMessage(bot, chatID, "❌ Error API ZiVPN setelah bayar: " + err.Error())
				return
			}
			if apiRes["success"].(bool) {
				data := apiRes["data"].(map[string]interface{})
				ipInfo, _ := getIpInfo()

				msg := fmt.Sprintf("✅ *PEMBAYARAN BERHASIL & AKUN DIBUAT*\n" +
					"━━━━━━━━━━━━━━━━━━━━━━━━━\n" +
					"🔑 *Password*: `%s`\n" +
					"🗓️ *Expired*: `%s`\n" +
					"📍 *Lokasi*: `%s`\n" +
					"📡 *ISP*: `%s`\n" +
					"━━━━━━━━━━━━━━━━━━━━━━━━━", password, data["expired"], ipInfo.City, ipInfo.Isp)

				reply := tgbotapi.NewMessage(chatID, msg)
				reply.ParseMode = "Markdown"
				bot.Send(reply)
			} else {
				sendMessage(bot, chatID, "❌ Gagal create akun: " + apiRes["message"].(string))
			}
			clearState(chatID)
			return
		}

		time.Sleep(5 * time.Second)
	}
	sendMessage(bot, chatID, "❌ Pembayaran timeout atau gagal. Coba lagi.")
	clearState(chatID)
}

// --- HELPER FUNCTIONS ---
func setState(userID int64, state string) {
	stateMutex.Lock()
	userStates[userID] = state
	stateMutex.Unlock()
}

func clearState(userID int64) {
	stateMutex.Lock()
	delete(userStates, userID)
	delete(tempData, userID)
	stateMutex.Unlock()
}

func loadTrialUsers() {
	file, err := ioutil.ReadFile(TrialDBFile)
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
	ioutil.WriteFile(TrialDBFile, []byte(strings.Join(lines, "\n")), 0644)
}

func generateRandomPassword(length int) string {
	chars := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var sb strings.Builder
	for i := 0; i < length; i++ {
		sb.WriteByte(chars[rand.Intn(len(chars))])
	}
	return sb.String()
}

// --- HANDLE CALLBACK ---
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
	default:
		sendMessage(bot, chatID, "Opsi tidak dikenal.")
	}
}

// Fungsi lain dari original (systemInfo, apiCall, dll.)
func apiCall(method string, endpoint string, body interface{}) (map[string]interface{}, error) {
	// ... (kode dari repo Anda)
}

func systemInfo(bot *tgbotapi.BotAPI, chatID int64) {
	// ... (kode dari repo Anda)
}

func getIpInfo() (IpInfo, error) {
	// Real implementasi jika ingin
	resp, err := http.Get("https://ipinfo.io/json")
	if err != nil {
		return IpInfo{"Unknown", "Unknown"}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var info struct {
		City string `json:"city"`
		Org  string `json:"org"`
	}
	json.Unmarshal(body, &info)
	return IpInfo{info.City, info.Org}, nil
}

// Tambahkan fungsi admin/backups/delete expired dari repo asli jika belum ada
func autoDeleteExpiredUsers(bot *tgbotapi.BotAPI, adminID int64) {
	// Implementasi dari original: cek users.db, hapus expired via API
}

func performAutoBackup(bot *tgbotapi.BotAPI, adminID int64) {
	// Implementasi dari original: backup users.db dan kirim ke admin
}

func handleRestoreFromUpload(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
	// Implementasi dari original: download file, restore users.db
}

func sendAndTrack(bot *tgbotapi.BotAPI, msg tgbotapi.Chattable) {
	// Implementasi dari original: send dan track message ID
}

func loadConfig() (BotConfig, error) {
	var c BotConfig
	data, err := ioutil.ReadFile(BotConfigFile)
	if err != nil {
		return c, err
	}
	json.Unmarshal(data, &c)
	return c, nil
}
