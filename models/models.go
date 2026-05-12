// models/models.go — Tipovačka 2.0
// Go structs odpovídající DB schématu.
package models

import "time"

// ─── Konstanty bodování ───────────────────────────────────────────────────────

const (
	PointsExact  = 3
	PointsWinner = 1
	PointsMiss   = 0
)

// ─── User ─────────────────────────────────────────────────────────────────────

type User struct {
	ID            int
	Username      string
	Email         *string
	FirstName     *string
	LastName      *string
	PasswordHash  string
	IsAdmin       bool
	IsOwner       bool
	IsHidden      bool
	NotifyAccess  bool
	IsInactive    bool
	IsApproved    bool
	IsBlocked     bool
	BackgroundURL *string
	UISettings    *string // JSON: {"exact_bg":"#...","winner_bg":"#...",...}
	Lang          string
	CreatedAt     time.Time
	LastLogin     *time.Time
}

func (u *User) DisplayName() string {
	if u.FirstName != nil && u.LastName != nil {
		return *u.FirstName + " " + *u.LastName
	}
	if u.FirstName != nil {
		return *u.FirstName
	}
	return u.Username
}

func (u *User) Role() string {
	if u.IsOwner {
		return "owner"
	}
	if u.IsAdmin {
		return "admin"
	}
	return "user"
}

// ─── Competition ──────────────────────────────────────────────────────────────

type Competition struct {
	ID        int
	Name      string
	Season    string
	IsActive  bool
	Sport     string
	SortOrder *int
	FdCode    string // football-data.org kód pro auto-fetch výsledků (např. CL, PL)
}

// ─── Round ────────────────────────────────────────────────────────────────────

type Round struct {
	ID            int
	CompetitionID int
	Name          string
	Deadline      *time.Time
	IsActive      bool
}

// ─── Team ─────────────────────────────────────────────────────────────────────

type Team struct {
	ID            int
	Name          string
	Sport         string
	Alias         *string
	DisplayName   *string
	LogoURL       *string
	Category      *string
	CompetitionID *int
}

func (t *Team) Display() string {
	if t.DisplayName != nil && *t.DisplayName != "" {
		return *t.DisplayName
	}
	return t.Name
}

// ─── Match ────────────────────────────────────────────────────────────────────

type Match struct {
	ID           int
	RoundID      int
	HomeTeamID   int
	AwayTeamID   int
	HomeScore    *int
	AwayScore    *int
	MatchDate    *time.Time
	IsFinished   bool
	NotifySent   bool
	// Joined fields (přes JOIN, ne vždy naplněny)
	HomeTeam     *Team
	AwayTeam     *Team
	Round        *Round
	// Flattened JOIN fields (rychlé query bez objektů)
	HomeTeamName string
	AwayTeamName string
	RoundName    string
}

// ─── Tip ──────────────────────────────────────────────────────────────────────

type Tip struct {
	ID        int
	UserID    int
	MatchID   int
	HomeScore int
	AwayScore int
	Points    *int
	CreatedAt time.Time
	// Joined
	User  *User
	Match *Match
}

// CalculatePoints vrátí body za tip (3 = přesný, 1 = správný vítěz, 0 = miss).
func (t *Tip) CalculatePoints(actualHome, actualAway int) int {
	if t.HomeScore == actualHome && t.AwayScore == actualAway {
		return PointsExact
	}
	if winner(t.HomeScore, t.AwayScore) == winner(actualHome, actualAway) {
		return PointsWinner
	}
	return PointsMiss
}

func winner(home, away int) string {
	if home > away {
		return "home"
	}
	if away > home {
		return "away"
	}
	return "draw"
}

// ─── ExtraQuestion ────────────────────────────────────────────────────────────

type ExtraQuestion struct {
	ID            int
	CompetitionID int
	OrderNum      int
	Text          string
	MaxPoints     int
	CorrectAnswer *string
	IsClosed      bool
	AnswerOptions *string // newline-separated dropdown options; nil = free-text input
}

// ─── ExtraAnswer ──────────────────────────────────────────────────────────────

type ExtraAnswer struct {
	ID         int
	QuestionID int
	UserID     int
	Answer     string
	Points     *int
	CreatedAt  time.Time
	// Joined
	User     *User
	Question *ExtraQuestion
}

// ─── AuditLog ─────────────────────────────────────────────────────────────────

type AuditLog struct {
	ID            int
	Timestamp     *time.Time
	AdminID       *int
	AdminUsername string
	Action        string
	EntityType    string
	EntityID      *int
	Description   string
	OldValue      *string // JSON
	NewValue      *string // JSON
	Undone        bool
}

// ─── CompetitionTeam (M2M) ───────────────────────────────────────────────────

type CompetitionTeam struct {
	ID            int
	CompetitionID int
	TeamID        int
}

// ─── CompetitionStandings ─────────────────────────────────────────────────────

type CompetitionStandings struct {
	ID            int
	CompetitionID int
	UserID        int
	TipPoints     int
	ExtraPoints   int
	GrandTotal    int
	ExactCount    int
	PartialCount  int
	MissCount     int
	UpdatedAt     time.Time
}

// ─── NotificationSetting ─────────────────────────────────────────────────────

type NotificationSetting struct {
	ID            int
	UserID        int
	CompetitionID int
}

// ─── PushSubscription ────────────────────────────────────────────────────────

type PushSubscription struct {
	ID        int
	UserID    int
	Endpoint  string
	P256dh    string
	Auth      string
	CreatedAt time.Time
}

// ─── PasswordResetToken ───────────────────────────────────────────────────────

type PasswordResetToken struct {
	ID        int
	UserID    int
	Token     string
	ExpiresAt time.Time
	Used      bool
	CreatedAt time.Time
}

// ─── SiteConfig ───────────────────────────────────────────────────────────────

type SiteConfig struct {
	Key   string
	Value string
}

// ─── UserGroup ────────────────────────────────────────────────────────────────

type UserGroup struct {
	ID              int
	Name            string
	CanSeeHidden    bool
	CanSeeDeadline  bool
}

// ─── GroupMembership ─────────────────────────────────────────────────────────

type GroupMembership struct {
	ID      int
	GroupID int
	UserID  int
}

// ─── Pomocné typy pro leaderboard ────────────────────────────────────────────

type UserRow struct {
	User             *User
	Total            int
	Extra            int
	GrandTotal       int
	Exact            int
	Winner           int
	Miss             int
	TipCount         int
	FinishedTipCount int
	Accuracy         *int
	TipRatio         string
	Place            int
	Trend            int
	Streak           int // consecutive exact tips (most recent, from last finished match backwards)
}

// ─── ExtraCol (pro leaderboard) ───────────────────────────────────────────────

type ExtraCol struct {
	QID            int
	SubIndex       int
	ColKey         string
	SubText        string
	MaxPoints      int
	IsLast         bool
	CorrectAnswers []string
	IsClosed       bool
}
