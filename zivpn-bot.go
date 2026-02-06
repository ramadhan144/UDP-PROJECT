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
    qrcode "github.com/skip2/go-qrcode"
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
)

var (
    PakasirSlug   = "zivpn_pay"  // e.g., "vpn-udp"
    PakasirApiKey = "S9zahTbmw4V7rjn2R9ctWRWljWYUjZxN"
    TrialDuration = 1  // hari untuk trial
    TrialTrackerFile = "/etc/zivpn/trial_tracker.json"  // file untuk track ID yang sudah trial
    trialTracker     = make(map[int64]bool)  // in-memory map
)

// Load trial tracker from file
func loadTrialTracker() {
    data, err := os.ReadFile(TrialTrackerFile)
    if err == nil {
        json.Unmarshal(data, &trialTracker)
    }
}

// Save trial tracker to file
func saveTrialTracker() {
    data, _ := json.Marshal(trialTracker)
    os.WriteFile(TrialTrackerFile, data, 0644)
}

func main() {
    startTime = time.Now() // Set waktu mulai bot
    rand.Seed(time.Now().UnixNano())

    if err := os.MkdirAll(BackupDir, 0755); err != nil {
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

    loadTrialTracker() // Load trial tracker after config

    bot, err := tgbotapi.NewBotAPI(config.BotToken)
    if err != nil {
        log.Panic(err)
    }

    bot.Debug = false
    log.Printf("Authorized on account %s", bot.Self.UserName)

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
            handleMessage(bot, update.Message, config.AdminID)
        } else if update.CallbackQuery != nil {
            handleCallback(bot, update.CallbackQuery, config.AdminID)
        }
    }
}

// --- HANDLE MESSAGE ---
func handleMessage(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, adminID int64) {
    userID := msg.From.ID
    isAdminUser := userID == adminID

    stateMutex.RLock()
    state, exists := userStates[userID]
    stateMutex.RUnlock()

    // Handle Restore dari Upload File (admin only)
    if exists && state == "wait_restore_file" {
        if isAdminUser && msg.Document != nil {
            handleRestoreFromUpload(bot, msg)
        } else {
            sendMessage(bot, msg.Chat.ID, "Please send a backup file (.json).")
        }
        return
    }

    if exists {
        handleState(bot, msg, state)
        return
    }

    text := strings.ToLower(msg.Text)

    if msg.IsCommand() {
        switch msg.Command() {
        case "start":
            if isAdminUser {
                showMainMenu(bot, msg.Chat.ID)
            } else {
                showPublicMenu(bot, msg.Chat.ID)
            }
        case "trial":
            handleTrialRequest(bot, msg)
        case "create":
            handleCreatePaid(bot, msg)
        case "info":
            handleServerInfo(bot, msg)
        // Admin-only commands
        case "panel", "menu":
            if isAdminUser {
                showMainMenu(bot, msg.Chat.ID)
            } else {
                sendMessage(bot, msg.Chat.ID, "Akses ditolak. Hanya admin.")
            }
        case "setgroup":
            if isAdminUser {
                args := msg.CommandArguments()
                if args == "" {
                    sendMessage(bot, msg.Chat.ID, "Format salah.\n\nUsage: `/setgroup <group_id>`\nContoh: /setgroup -1001234567890")
                    return
                }
                groupID, err := strconv.ParseInt(args, 10, 64)
                if err != nil {
                    sendMessage(bot, msg.Chat.ID, "Group ID harus berupa angka.")
                    return
                }
                updateConfig(func(config *BotConfig) {
                    config.NotifGroupID = groupID
                })
                sendMessage(bot, msg.Chat.ID, "Group ID notifikasi berhasil diupdate!")
            } else {
                sendMessage(bot, msg.Chat.ID, "Akses ditolak. Hanya admin.")
            }
        // Tambahkan command admin lain sesuai kode asli...
        default:
            if isAdminUser {
                // Handle other commands as per original
            } else {
                sendMessage(bot, msg.Chat.ID, "Command tidak dikenal.")
            }
        }
        return
    }
    // ... handle other text if needed ...
}

// --- HANDLE CALLBACK ---
func handleCallback(bot *tgbotapi.BotAPI, cq *tgbotapi.CallbackQuery, adminID int64) {
    userID := cq.From.ID
    isAdminUser := userID == adminID
    data := cq.Data

    if strings.HasPrefix(data, "public_") && !isAdminUser {
        switch data {
        case "public_trial":
            handleTrialRequest(bot, cq.Message)
        case "public_create":
            handleCreatePaid(bot, cq.Message)
        case "public_info":
            handleServerInfo(bot, cq.Message)
        }
        bot.AnswerCallbackQuery(tgbotapi.NewCallback(cq.ID, ""))
        return
    }
    // ... existing admin callbacks from original code ...
    bot.AnswerCallbackQuery(tgbotapi.NewCallback(cq.ID, ""))
}

// Public menu with buttons
func showPublicMenu(bot *tgbotapi.BotAPI, chatID int64) {
    msg := tgbotapi.NewMessage(chatID, "Selamat datang! Pilih opsi:")
    keyboard := tgbotapi.NewInlineKeyboardMarkup(
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("Buat Akun Trial (1x)", "public_trial"),
            tgbotapi.NewInlineKeyboardButtonData("Buat Akun Berbayar", "public_create"),
        ),
        tgbotapi.NewInlineKeyboardRow(
            tgbotapi.NewInlineKeyboardButtonData("Info Server", "public_info"),
        ),
    )
    msg.ReplyMarkup = keyboard
    sendAndTrack(bot, msg)
}

func handleTrialRequest(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
    userID := msg.From.ID
    if trialTracker[userID] {
        sendMessage(bot, msg.Chat.ID, "Kamu sudah menggunakan trial sekali. Silakan buat akun berbayar.")
        return
    }

    // Masuk state untuk input password
    stateMutex.Lock()
    userStates[userID] = "trial_password"
    stateMutex.Unlock()
    sendMessage(bot, msg.Chat.ID, "Masukkan password untuk akun trial (bebas):")
}

func handleCreatePaid(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
    // Masuk state untuk input password
    stateMutex.Lock()
    userStates[msg.From.ID] = "paid_password"
    tempUserData[msg.From.ID] = make(map[string]string)  // simpan temp data
    stateMutex.Unlock()
    sendMessage(bot, msg.Chat.ID, "Masukkan password untuk akun (bebas):")
}

func handleServerInfo(bot *tgbotapi.BotAPI, msg *tgbotapi.Message) {
    // Asumsi config punya Domain, adjust jika beda
    config, _ := loadConfig()
    info := fmt.Sprintf("Server Info:\nDomain: %s\nPort UDP: 5667\nGunakan app seperti HTTP Custom untuk connect.", config.Domain) // Ganti dengan actual field jika ada
    sendMessage(bot, msg.Chat.ID, info)
}

// Tambah ke handleState (asumsi fungsi ini ada di kode asli, tambah case)
func handleState(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, state string) {
    switch state {
    case "trial_password":
        password := msg.Text
        // Call API to create user with 1 day exp
        params := map[string]string{
            "password": password,
            "expired":  fmt.Sprintf("%d", TrialDuration),
        }
        err := createUser(bot, msg.Chat.ID, params)  // reuse existing createUser, asumsi ada
        if err != nil {
            sendMessage(bot, msg.Chat.ID, "Gagal buat trial: " + err.Error())
            return
        }
        // Mark as used
        trialTracker[msg.From.ID] = true
        saveTrialTracker()
        sendMessage(bot, msg.Chat.ID, "Akun trial dibuat! Expired dalam 1 hari. Gunakan password: " + password)
        // Kirim info server
        handleServerInfo(bot, msg)
        // Clear state
        stateMutex.Lock()
        delete(userStates, msg.From.ID)
        stateMutex.Unlock()
    case "paid_password":
        password := msg.Text
        tempUserData[msg.From.ID]["password"] = password

        // Generate order_id unique (e.g., timestamp + userID)
        orderID := fmt.Sprintf("ORDER-%d-%d", time.Now().Unix(), msg.From.ID)

        // Call Pakasir API for QRIS
        payload := map[string]interface{}{
            "project": PakasirSlug,
            "order_id": orderID,
            "amount": 50000,  // harga fixed, ubah sesuai
            "api_key": PakasirApiKey,
        }
        jsonPayload, _ := json.Marshal(payload)
        req, _ := http.NewRequest("POST", "https://app.pakasir.com/api/transactioncreate/qris", bytes.NewBuffer(jsonPayload))
        req.Header.Set("Content-Type", "application/json")
        client := &http.Client{}
        resp, err := client.Do(req)
        if err != nil {
            sendMessage(bot, msg.Chat.ID, "Gagal generate QRIS: " + err.Error())
            return
        }
        defer resp.Body.Close()
        body, _ := io.ReadAll(resp.Body)
        var result map[string]interface{}
        json.Unmarshal(body, &result)
        payment := result["payment"].(map[string]interface{})
        paymentNumber := payment["payment_number"].(string)

        // Generate QR code image from paymentNumber
        qr, _ := qrcode.New(paymentNumber, qrcode.Medium)
        qrFile := fmt.Sprintf("/tmp/qr_%s.png", orderID)
        qr.WriteFile(256, qrFile)

        // Kirim QR ke user
        photo := tgbotapi.NewPhoto(msg.Chat.ID, tgbotapi.FilePath(qrFile))
        photo.Caption = fmt.Sprintf("Scan QRIS ini untuk bayar Rp%d. Order ID: %s\nExpired: %s", int(payment["amount"].(float64)), orderID, payment["expired_at"])
        bot.Send(photo)
        os.Remove(qrFile)  // cleanup

        // Masuk state polling status
        stateMutex.Lock()
        tempUserData[msg.From.ID]["order_id"] = orderID
        tempUserData[msg.From.ID]["amount"] = fmt.Sprintf("%d", int(payment["amount"].(float64)))
        userStates[msg.From.ID] = "paid_waiting"
        stateMutex.Unlock()

        // Start polling in goroutine
        go pollPaymentStatus(bot, msg.Chat.ID, msg.From.ID, orderID, int(payment["amount"].(float64)))
    // ... other cases from original ...
    }
}

func pollPaymentStatus(bot *tgbotapi.BotAPI, chatID int64, userID int64, orderID string, amount int) {
    for i := 0; i < 180; i++ {  // poll 30 menit (10s x 180)
        time.Sleep(10 * time.Second)
        url := fmt.Sprintf("https://app.pakasir.com/api/transactiondetail?project=%s&amount=%d&order_id=%s&api_key=%s", PakasirSlug, amount, orderID, PakasirApiKey)
        resp, err := http.Get(url)
        if err != nil {
            continue
        }
        body, _ := io.ReadAll(resp.Body)
        resp.Body.Close()
        var result map[string]interface{}
        json.Unmarshal(body, &result)
        transaction := result["transaction"].(map[string]interface{})
        if transaction["status"] == "completed" {
            // Create user via API
            params := map[string]string{
                "password": tempUserData[userID]["password"],
                "expired":  "30",  // misal 30 hari untuk paid
            }
            createUser(bot, chatID, params)
            sendMessage(bot, chatID, "Pembayaran confirmed! Akun dibuat dengan password: " + params["password"])
            // Clear state
            stateMutex.Lock()
            delete(userStates, userID)
            delete(tempUserData, userID)
            stateMutex.Unlock()
            return
        }
    }
    sendMessage(bot, chatID, "Pembayaran expired. Coba lagi.")
    // Clear state
    stateMutex.Lock()
    delete(userStates, userID)
    delete(tempUserData, userID)
    stateMutex.Unlock()
}

// Asumsi fungsi-fungsi lain seperti loadConfig, sendMessage, createUser, autoDeleteExpiredUsers, performAutoBackup, handleRestoreFromUpload, showMainMenu, dll. tetap dari kode asli.
// Tambahkan fungsi yang hilang jika diperlukan, tapi ini adalah modifikasi utama.
