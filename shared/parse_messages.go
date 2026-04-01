package shared

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// ── CSV ───────────────────────────────────────────────────────────────────────

// ParseCSVMessage parses PODIUM-* and SCOREGEN-* UDP messages from ports 51520
// and 23467 into a ProScoreMessage.
func ParseCSVMessage(server, data string) (ProScoreMessage, error) {
	fields := SplitCSV(strings.TrimSpace(data))
	if len(fields) == 0 {
		return ProScoreMessage{}, fmt.Errorf("empty CSV message")
	}

	msg := ProScoreMessage{
		Time:        time.Now().UnixMilli(),
		Server:      server,
		FullMessage: fields,
	}

	cmd := fields[0]
	switch cmd {
	case "PODIUM-STATUS":
		// fields: cmd, statusCode, apparatus, competitor, firstName, surname, club, flag
		if len(fields) < 2 {
			return ProScoreMessage{}, fmt.Errorf("PODIUM-STATUS too short")
		}
		switch fields[1] {
		case "0":
			msg.Status = "scoring"
		case "1":
			msg.Status = "competing"
		default:
			msg.Status = "ready"
		}
		if len(fields) > 2 {
			msg.Apparatus = Apparatus[fields[2]]
		}
		if len(fields) > 3 {
			msg.Competitor = fields[3]
		}
		if len(fields) > 5 {
			msg.Name = strings.TrimSpace(StripQuotes(fields[4]) + " " + StripQuotes(fields[5]))
		}
		if len(fields) > 6 {
			msg.Club = StripQuotes(fields[6])
		}

	case "PODIUM-SCORE":
		// PODIUM-SCORE,VT,102,"Rebecca","Hale","MLC",false,12.550
		msg.Status = "stopped"
		if len(fields) > 1 {
			msg.Apparatus = Apparatus[fields[1]]
		}
		if len(fields) > 2 {
			msg.Competitor = fields[2]
		}
		if len(fields) > 4 {
			msg.Name = strings.TrimSpace(StripQuotes(fields[3]) + " " + StripQuotes(fields[4]))
		}
		if len(fields) > 5 {
			msg.Club = StripQuotes(fields[5])
		}
		if len(fields) > 7 {
			if f, err := strconv.ParseFloat(StripQuotes(fields[7]), 64); err == nil {
				msg.FinalScore = f
			}
		}

	case "PODIUM-CLEAR":
		msg.Status = "stopped"
		if len(fields) > 1 {
			msg.Apparatus = Apparatus[fields[1]]
		}

	case "SCOREGEN-LAST":
		// SCOREGEN-LAST,1,apparatus,competitor,"Name",group,E1..E6,D,E,ND,Final
		msg.Status = "stopped"
		if len(fields) > 2 {
			msg.Apparatus = Apparatus[fields[2]]
		}
		if len(fields) > 3 {
			msg.Competitor = fields[3]
		}
		if len(fields) > 4 {
			msg.Name = StripQuotes(fields[4])
		}
		if len(fields) > 12 {
			if f, err := strconv.ParseFloat(fields[12], 64); err == nil {
				msg.DScore = f
			}
		}
		if len(fields) > 13 {
			if f, err := strconv.ParseFloat(fields[13], 64); err == nil {
				msg.EScore = f
			}
		}
		if len(fields) > 14 {
			if f, err := strconv.ParseFloat(fields[14], 64); err == nil {
				msg.ND = f
			}
		}
		if len(fields) > 15 {
			if f, err := strconv.ParseFloat(fields[15], 64); err == nil {
				msg.FinalScore = f
			}
		}

	default:
		return ProScoreMessage{}, fmt.Errorf("unknown message type: %q", cmd)
	}

	return msg, nil
}

// SplitCSV splits a comma-separated string, respecting double-quoted fields.
func SplitCSV(s string) []string {
	var fields []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '"':
			inQuote = !inQuote
			cur.WriteByte(ch)
		case ch == ',' && !inQuote:
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	fields = append(fields, cur.String())
	return fields
}

// StripQuotes removes all double-quote characters from s.
func StripQuotes(s string) string {
	return strings.ReplaceAll(s, `"`, "")
}

// ── XML ───────────────────────────────────────────────────────────────────────

// ParseXMLMessage parses an XML UDP message from port 51521.
// Recognised root elements: NowUp (status=competing) and NewScore (status=stopped).
// The enrichScore callback is optional — when non-nil it is called for NewScore
// messages and may populate additional score fields (e.g. from the ProScore HTTP
// API). Pass nil to skip enrichment.
func ParseXMLMessage(server, data string, enrichScore func(*ProScoreMessage)) (ProScoreMessage, error) {
	decoder := xml.NewDecoder(strings.NewReader(data))
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		// Accept ISO-8859-1 as-is; content is ASCII-range in practice.
		return input, nil
	}

	// Advance past the XML declaration and whitespace to the root StartElement.
	for {
		token, err := decoder.Token()
		if err != nil {
			return ProScoreMessage{}, fmt.Errorf("XML parse: finding root element: %w", err)
		}
		se, ok := token.(xml.StartElement)
		if !ok {
			continue
		}

		rootName := se.Name.Local
		child, err := xmlNodeToMap(decoder)
		if err != nil {
			return ProScoreMessage{}, fmt.Errorf("XML parse: %w", err)
		}
		if len(se.Attr) > 0 {
			attrs := make(map[string]string, len(se.Attr))
			for _, a := range se.Attr {
				attrs[a.Name.Local] = a.Value
			}
			child["_attr"] = attrs
		}
		root := map[string]any{rootName: child}

		msg := ProScoreMessage{
			Time:        time.Now().UnixMilli(),
			Server:      server,
			FullMessage: root,
		}

		switch rootName {
		case "NowUp":
			attrs := xmlAttrs(child)
			if attrs["Num"] == "" {
				msg.Status = "stopped" //Gives empty nowup if cancel a score
			} else {
				msg.Status = "competing"
			}
			msg.Apparatus = attrs["Event"]
			msg.Competitor = attrs["Num"]
			msg.Name = strings.TrimSpace(attrs["FName"] + " " + attrs["LName"])
			msg.Club = xmlStr(child, "Gym")

		case "NewScore":
			attrs := xmlAttrs(child)
			msg.Status = "stopped"
			msg.Apparatus = attrs["Event"]
			msg.Competitor = attrs["Num"]
			msg.Name = strings.TrimSpace(attrs["FName"] + " " + attrs["LName"])
			msg.Club = xmlStr(child, "Gym")
			msg.Level = attrs["Level"]
			if f, err := strconv.ParseFloat(attrs["Score"], 64); err == nil {
				msg.FinalScore = f
			}
			if enrichScore != nil {
				enrichScore(&msg)
			}

		default:
			msg.Status = "unknown"
		}

		return msg, nil
	}
}

// XMLToJSON converts an XML string to its nested-map JSON representation.
func XMLToJSON(xmlData string) (string, error) {
	decoder := xml.NewDecoder(strings.NewReader(xmlData))
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		return input, nil
	}
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", fmt.Errorf("XML parse error: finding root: %w", err)
		}
		if se, ok := token.(xml.StartElement); ok {
			child, err := xmlNodeToMap(decoder)
			if err != nil {
				return "", fmt.Errorf("XML parse error: %w", err)
			}
			if len(se.Attr) > 0 {
				attrs := make(map[string]string, len(se.Attr))
				for _, a := range se.Attr {
					attrs[a.Name.Local] = a.Value
				}
				child["_attr"] = attrs
			}
			root := map[string]any{se.Name.Local: child}
			b, err := json.Marshal(root)
			if err != nil {
				return "", fmt.Errorf("JSON encode error: %w", err)
			}
			return string(b), nil
		}
	}
}

func xmlAttrs(node map[string]any) map[string]string {
	result := map[string]string{}
	if attrs, ok := node["_attr"].(map[string]string); ok {
		for k, v := range attrs {
			result[k] = v
		}
	}
	return result
}

func xmlStr(node map[string]any, key string) string {
	if child, ok := node[key].(map[string]any); ok {
		if text, ok := child["_text"].(string); ok {
			return text
		}
	}
	return ""
}

func xmlNodeToMap(decoder *xml.Decoder) (map[string]any, error) {
	result := make(map[string]any)
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch t := token.(type) {
		case xml.StartElement:
			child, err := xmlNodeToMap(decoder)
			if err != nil {
				return nil, err
			}
			if len(t.Attr) > 0 {
				attrs := make(map[string]string, len(t.Attr))
				for _, a := range t.Attr {
					attrs[a.Name.Local] = a.Value
				}
				child["_attr"] = attrs
			}
			if existing, ok := result[t.Name.Local]; ok {
				switch v := existing.(type) {
				case []any:
					result[t.Name.Local] = append(v, child)
				default:
					result[t.Name.Local] = []any{v, child}
				}
			} else {
				result[t.Name.Local] = child
			}
		case xml.CharData:
			if text := strings.TrimSpace(string(t)); text != "" {
				result["_text"] = text
			}
		case xml.EndElement:
			return result, nil
		}
	}
	return result, nil
}
