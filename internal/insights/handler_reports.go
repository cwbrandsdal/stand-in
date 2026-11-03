package insights

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	defaultThreadLimit = 50
	maxThreadLimit     = 500
	maxJSONBody        = 64 << 10
	maxMonthlyUSD      = 100000
)

func handleThreads(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	from, to, ok := parseRange(c)
	if !ok {
		return
	}
	limit := queryInt(c, "limit", defaultThreadLimit)
	if limit <= 0 || limit > maxThreadLimit {
		limit = defaultThreadLimit
	}
	rows, errThreads := svc.store.threads(c.Request.Context(), from, to, c.Query("sort") != "recent", limit)
	if errThreads != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errThreads.Error()})
		return
	}
	if rows == nil {
		rows = []ThreadRow{}
	}
	c.JSON(http.StatusOK, gin.H{"threads": rows})
}

func handleThread(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	session := strings.TrimSpace(c.Query("session"))
	if session == "" || len(session) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session is required"})
		return
	}
	rows, errRows := svc.store.threadRequests(c.Request.Context(), session)
	if errRows != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errRows.Error()})
		return
	}
	if rows == nil {
		rows = []ThreadRequest{}
	}
	c.JSON(http.StatusOK, gin.H{"session_id": session, "requests": rows, "truncated": len(rows) >= maxThreadRequests})
}

func handleWaste(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	from, to, ok := parseRange(c)
	if !ok {
		return
	}
	report, errWaste := svc.store.waste(c.Request.Context(), from, to)
	if errWaste != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errWaste.Error()})
		return
	}
	c.JSON(http.StatusOK, report)
}

func handleAccounts(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	from, to, ok := parseRange(c)
	if !ok {
		return
	}
	bucket, offset := parseBucket(c, from, to)
	report, errReport := svc.store.accountsReport(c.Request.Context(), from, to, bucket, offset, time.Now().UTC())
	if errReport != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errReport.Error()})
		return
	}
	c.JSON(http.StatusOK, report)
}

func handleHealth(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	from, to, ok := parseRange(c)
	if !ok {
		return
	}
	bucket, offset := parseBucket(c, from, to)
	report, errReport := svc.store.health(c.Request.Context(), from, to, bucket, offset)
	if errReport != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errReport.Error()})
		return
	}
	c.JSON(http.StatusOK, report)
}

func handleAlerts(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	limit := queryInt(c, "limit", defaultLimit)
	if limit <= 0 || limit > maxAlertsListed {
		limit = defaultLimit
	}
	alerts, errAlerts := svc.store.alerts(c.Request.Context(), int64(queryInt(c, "before", 0)), limit)
	if errAlerts != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errAlerts.Error()})
		return
	}
	if alerts == nil {
		alerts = []Alert{}
	}
	settings, errSettings := svc.store.alertSettings(c.Request.Context())
	if errSettings != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errSettings.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"alerts": alerts, "settings": settings, "luna": lunaStatus()})
}

func handleAlertSettings(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	var settings AlertSettings
	if !bindJSON(c, &settings) {
		return
	}
	if errSave := svc.store.saveAlertSettings(c.Request.Context(), settings); errSave != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errSave.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"settings": settings})
}

func handleAlertAck(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	var body struct {
		ID  int64 `json:"id"`
		All bool  `json:"all"`
	}
	if !bindJSON(c, &body) {
		return
	}
	if body.ID <= 0 && !body.All {
		c.JSON(http.StatusBadRequest, gin.H{"error": "set id or all"})
		return
	}
	if body.All {
		body.ID = 0
	}
	count, errAck := svc.store.ackAlerts(c.Request.Context(), body.ID, time.Now().UTC())
	if errAck != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errAck.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"acknowledged": count})
}

func handleAlertTest(c *gin.Context) {
	if running(c) == nil {
		return
	}
	if _, configured := lunaTarget(); !configured {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Luna is not configured: set INSIGHTS_LUNA_ALERTS_URL and the hub's Luna alert token (see ops/windows/README.md)"})
		return
	}
	if errSend := sendLunaTest(c.Request.Context(), time.Now().UTC()); errSend != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": errSend.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"sent": true, "server": lunaStatus().Server})
}

func handleOverview(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	overview, errOverview := svc.overview(c.Request.Context(), time.Now().UTC())
	if errOverview != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errOverview.Error()})
		return
	}
	c.JSON(http.StatusOK, overview)
}

func handleAccountPrice(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	var body struct {
		AuthID     string   `json:"auth_id"`
		MonthlyUSD *float64 `json:"monthly_usd"`
	}
	if !bindJSON(c, &body) {
		return
	}
	body.AuthID = strings.TrimSpace(body.AuthID)
	if body.AuthID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_id is required"})
		return
	}
	if body.MonthlyUSD != nil && (math.IsNaN(*body.MonthlyUSD) || *body.MonthlyUSD < 0 || *body.MonthlyUSD > maxMonthlyUSD) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "monthly_usd must be between 0 and 100000, or null to use the plan default"})
		return
	}
	found, errSet := svc.store.setMonthlyPrice(c.Request.Context(), body.AuthID, body.MonthlyUSD)
	if errSet != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errSet.Error()})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown account"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"auth_id": body.AuthID, "monthly_usd": body.MonthlyUSD})
}

func handleBackup(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	path, errBackup := svc.backupNow(c.Request.Context(), time.Now().UTC())
	status := svc.backupStatus(c.Request.Context())
	if errBackup != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errBackup.Error(), "backup": status})
		return
	}
	c.JSON(http.StatusOK, gin.H{"path": path, "backup": status})
}

func handleSQLSchema(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	tables, errSchema := svc.store.schemaInfo(c.Request.Context())
	if errSchema != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errSchema.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tables": tables})
}

func handleSQL(c *gin.Context) {
	svc := running(c)
	if svc == nil {
		return
	}
	var body struct {
		Query string `json:"query"`
	}
	if !bindJSON(c, &body) {
		return
	}
	result, errRun := svc.store.runSQL(c.Request.Context(), body.Query)
	if errRun != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errRun.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

// bindJSON decodes a small JSON body, answering 400 itself when it cannot.
func bindJSON(c *gin.Context, target any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxJSONBody)
	if errBind := c.ShouldBindJSON(target); errBind != nil {
		message := "invalid JSON body"
		var tooLarge *http.MaxBytesError
		if errors.As(errBind, &tooLarge) {
			message = "request body too large"
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": message})
		return false
	}
	return true
}

// priceSourceLabel names where prices came from without exposing local file paths.
func priceSourceLabel(source string) string {
	if source == "embedded" || source == "" {
		return "built-in table"
	}
	if source == defaultPricesURL {
		return "LiteLLM (daily)"
	}
	if parsed, errParse := url.Parse(source); errParse == nil && parsed.Host != "" {
		return parsed.Host
	}
	return "local file"
}
