package shared

// ProScoreMessage is the unified structure used by both programs.
// The scoring listener parses UDP traffic into this type and POSTs it to the
// video server. The video server receives it via /events and persists it.
type ProScoreMessage struct {
	Time        int64   `json:"time"`
	Server      string  `json:"server"`
	Status      string  `json:"status"`
	Apparatus   string  `json:"apparatus"`
	Competitor  string  `json:"competitor"`
	Name        string  `json:"name,omitempty"`
	Club        string  `json:"club,omitempty"`
	Level       string  `json:"level,omitempty"`
	DScore      float64 `json:"dscore,omitempty"`
	EScore      float64 `json:"escore,omitempty"`
	ND          float64 `json:"nd,omitempty"`
	FinalScore  float64 `json:"finalScore,omitempty"`
	Score1      float64 `json:"score1,omitempty"`
	DScore2     float64 `json:"dscore2,omitempty"`
	EScore2     float64 `json:"escore2,omitempty"`
	ND2         float64 `json:"nd2,omitempty"`
	Score2      float64 `json:"score2,omitempty"`
	FullMessage any     `json:"fullMessage"`
}

// EventMsg is the read-side view of a routine row returned by the video
// server's /eventlist and /scorelist endpoints.
type EventMsg struct {
	ID         *int64  `json:"id,omitempty"`
	Server     string  `json:"server"`
	Apparatus  string  `json:"apparatus"`
	Competitor string  `json:"competitor"`
	Name       string  `json:"name"`
	Club       string  `json:"club,omitempty"`
	TimeStart  int64   `json:"time_start,omitempty"`
	TimeStop   *int64  `json:"time_stop,omitempty"`
	TimeScore  *int64  `json:"time_score,omitempty"`
	TimeScore2 *int64  `json:"time_score2,omitempty"`
	D          float64 `json:"d,omitempty"`
	E          float64 `json:"e,omitempty"`
	ND         float64 `json:"nd,omitempty"`
	FinalScore float64 `json:"final_score,omitempty"`
	Score1     float64 `json:"score1,omitempty"`
	D2         float64 `json:"d2,omitempty"`
	E2         float64 `json:"e2,omitempty"`
	ND2        float64 `json:"nd2,omitempty"`
	Score2     float64 `json:"score2,omitempty"`
	Status     string  `json:"status"`
}

// VideoFile describes a single uploaded video chunk as returned by /videolist.
type VideoFile struct {
	CameraDesc string `json:"camera_desc,omitempty"`
	Length     int64  `json:"length,omitempty"`
	EndTime    int64  `json:"end_time,omitempty"`
	StartTime  int64  `json:"start_time,omitempty"`
	Filename   string `json:"filename"`
	RoutineID  *int64 `json:"routine_id,omitempty"`
}
