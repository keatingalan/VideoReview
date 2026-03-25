package shared

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// CompetitorInfo holds the detailed score data retrieved from the ProScore HTTP API.
type CompetitorInfo struct {
	Num         int
	FirstName   string
	LastName    string
	Gym         string
	Level       string
	Session     string
	Score1      *float64
	StartValue1 *float64
	EScore1     *float64
	Adjust1     *float64
	Score2      *float64
	StartValue2 *float64
	EScore2     *float64
	Adjust2     *float64
}

// apparatusKeypadID maps apparatus display name → two-digit hex keypad ID.
// Pre-seeded with the standard defaults; updated at runtime as IDs are discovered.
var apparatusKeypadID = map[string]string{
	"Vault": "01",
	"Bars":  "02",
	"Beam":  "03",
	"Floor": "04",
}

var apparatusKeypadIDMu sync.Mutex

func nullableFloat(s string) *float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f == -99 {
		return nil
	}
	f = math.Round(f*1000) / 1000
	return &f
}

// parseProScoreResponse parses the semicolon-delimited key=value format used by
// the ProScore HTTP API. String values use the length-prefixed quoted form:
//
//	FName:S=6"Alyssa"
func parseProScoreResponse(body string) map[string]string {
	fields := make(map[string]string)
	for _, tok := range strings.Split(body, ";") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		eq := strings.Index(tok, "=")
		if eq == -1 {
			continue
		}
		keypart := tok[:eq]
		valpart := tok[eq+1:]

		key := keypart
		if colon := strings.Index(keypart, ":"); colon != -1 {
			key = keypart[:colon]
			if keypart[colon+1:] == "S" {
				if q := strings.Index(valpart, `"`); q != -1 {
					length, err := strconv.Atoi(valpart[:q])
					if err == nil && q+1+length <= len(valpart) {
						valpart = valpart[q+1 : q+1+length]
					}
				}
			}
		}
		fields[key] = valpart
	}
	return fields
}

func parseCompetitorResponse(body string) (*CompetitorInfo, error) {
	fields := parseProScoreResponse(body)
	if e, ok := fields["E"]; ok {
		return nil, fmt.Errorf("server error: %s", e)
	}
	info := &CompetitorInfo{}
	for k, v := range fields {
		switch k {
		case "Num":
			info.Num, _ = strconv.Atoi(v)
		case "FName":
			info.FirstName = v
		case "LName":
			info.LastName = v
		case "Gym":
			info.Gym = v
		case "Level":
			info.Level = v
		case "Session":
			info.Session = v
		case "Ave_Score1":
			info.Score1 = nullableFloat(v)
		case "Start_Value1":
			info.StartValue1 = nullableFloat(v)
		case "EScore1":
			info.EScore1 = nullableFloat(v)
		case "Adjust1":
			info.Adjust1 = nullableFloat(v)
		case "Ave_Score2":
			info.Score2 = nullableFloat(v)
		case "Start_Value2":
			info.StartValue2 = nullableFloat(v)
		case "EScore2":
			info.EScore2 = nullableFloat(v)
		case "Adjust2":
			info.Adjust2 = nullableFloat(v)
		}
	}
	return info, nil
}

func proScoreHTTPPost(body string, server string) (string, error) {
	var url = "http://" + server + ":51514/proscore"
	resp, err := http.Post(url, "text/plain", strings.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}
	return string(raw), nil
}

func proScoreInit(keypadID string, server string) (string, error) {
	body := fmt.Sprintf(
		`FC=init;RE=4;ID:S=%d"%s";Batt:S=2"AK";Version:S=11"VideoReview";Cmd:S=4"init";`,
		len(keypadID), keypadID,
	)
	raw, err := proScoreHTTPPost(body, server)
	if err != nil {
		return "", err
	}
	return parseProScoreResponse(raw)["Event"], nil
}

func findKeypadID(apparatusName string, server string) (string, error) {
	apparatusKeypadIDMu.Lock()
	knownID := apparatusKeypadID[apparatusName]
	apparatusKeypadIDMu.Unlock()

	if knownID != "" {
		event, err := proScoreInit(knownID, server)
		if err == nil && event == apparatusName {
			return knownID, nil
		}
		//appendLog(fmt.Sprintf("Keypad ID %q for %q failed (got %q), scanning 00–ff…", knownID, apparatusName, event))
	}

	for i := 0; i <= 0xff; i++ {
		candidate := fmt.Sprintf("%02x", i)
		event, err := proScoreInit(candidate, server)
		if err != nil {
			continue
		}
		if event != "" {
			apparatusKeypadIDMu.Lock()
			apparatusKeypadID[event] = candidate
			apparatusKeypadIDMu.Unlock()
			//appendLog(fmt.Sprintf("Discovered keypad ID %q → %q", candidate, event))
		}
		if event == apparatusName {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no keypad ID found for apparatus %q after scanning 00–ff", apparatusName)
}

// GetCompetitorInfoByHTTP fetches detailed competitor scores from the local
// ProScore HTTP API using a two-step init + getcompnum flow.
func GetCompetitorInfoByHTTP(num string, group int, apparatusName string, server string) (*CompetitorInfo, error) {
	keypadID, err := findKeypadID(apparatusName, server)
	if err != nil {
		return nil, err
	}
	compBody := fmt.Sprintf(
		`FC=getcompnum;RE=5;ID:S=%d"%s";Batt:S=2"AK";Version:S=11"VideoReview";Num:I=%s;Group:I=%d;`,
		len(keypadID), keypadID, num, group,
	)
	compRaw, err := proScoreHTTPPost(compBody, server)
	if err != nil {
		return nil, fmt.Errorf("getcompnum request: %w", err)
	}
	return parseCompetitorResponse(compRaw)
}

// EnrichFromProScore is passed as the enrichScore callback to ParseXMLMessage
// for NewScore messages. It fetches detailed scores from the local ProScore
// HTTP API and populates the message fields.
func EnrichFromProScore(msg *ProScoreMessage) {
	info, err := GetCompetitorInfoByHTTP(msg.Competitor, 1, msg.Apparatus, msg.Server)
	if err != nil {
		//appendLog(fmt.Sprintf("GetCompetitorInfo warning: %v", err))
		return
	}
	if info == nil {
		return
	}
	if info.StartValue1 != nil {
		msg.DScore = *info.StartValue1
	}
	if info.EScore1 != nil {
		msg.EScore = *info.EScore1
	}
	if info.Adjust1 != nil {
		msg.ND = *info.Adjust1
	}
	if info.Score1 != nil {
		msg.Score1 = *info.Score1
	}
	if info.StartValue2 != nil {
		msg.DScore2 = *info.StartValue2
	}
	if info.EScore2 != nil {
		msg.EScore2 = *info.EScore2
	}
	if info.Adjust2 != nil {
		msg.ND2 = *info.Adjust2
	}
	if info.Score2 != nil {
		msg.Score2 = *info.Score2
	}
	if info.Level != "" {
		msg.Level = info.Level
	}
	if info.Gym != "" {
		msg.Club = info.Gym
	}
}
