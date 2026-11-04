package speedlimit

import "encoding/json"

type RawLimit struct {
	Down    *int64          `json:"down"`
	Up      *int64          `json:"up"`
	Penalty json.RawMessage `json:"penalty"` // строка "name"/"none" или объект {"down":"..","up":".."}
}

type rawUser struct {
	Emails []string `json:"emails"`
	RawLimit
}

type rawStep struct {
	Above *int64  `json:"above"`
	For   *string `json:"for"`
	Limit *int64  `json:"limit"`
}

type rawRecover struct {
	Below *int64  `json:"below"`
	For   *string `json:"for"`
	After *string `json:"after"`
}

// rawDirProfile — секция одного направления профиля.
type rawDirProfile struct {
	Steps   []rawStep       `json:"steps"`
	Recover json.RawMessage `json:"recover"` // объект или строка "never"
}

// rawProfile: раздельный вид (Down/Up) ИЛИ краткий вид (Steps/Recover), не оба сразу (V09).
type rawProfile struct {
	Down *rawDirProfile `json:"down"`
	Up   *rawDirProfile `json:"up"`
	// краткий вид = одинаковая секция для обоих направлений
	Steps   []rawStep       `json:"steps"`
	Recover json.RawMessage `json:"recover"`
}

type rawUDP struct {
	MaxDelayMs *int `json:"maxDelayMs"`
}

type rawConfig struct {
	Enabled   *bool                  `json:"enabled"`
	Penalties *bool                  `json:"penalties"`
	BurstMs   *int                   `json:"burstMs"`
	UDP       *rawUDP                `json:"udp"`
	Default   *RawLimit              `json:"default"`
	Profiles  map[string]*rawProfile `json:"profiles"`
	Users     []rawUser              `json:"users"`
}

// Проверенные (разрешённые) типы, которые строит Parse

type step struct {
	aboveKbps, limitKbps int64
	forTicks             int // длительность окна в секундах (= тиков)
}

type recoverCfg struct {
	never      bool
	belowKbps  int64 // 0 = условие не задано
	forTicks   int
	afterTicks int // 0 = не задано
}

// dirProfile — штрафная секция ОДНОГО направления. nil = штрафа в этом направлении нет.
type dirProfile struct {
	steps []step
	rec   recoverCfg
}

// profile хранит секции обоих направлений. Краткий вид при разборе превращается
// в две одинаковые (но не разделяемые по указателю) секции.
type profile struct {
	name     string
	down, up *dirProfile // nil = у профиля нет секции для этого направления
}

type resolvedRule struct { // после наследования от default
	downKbps, upKbps int64
	downProf, upProf *dirProfile // nil = штрафа в этом направлении нет
}

type Config struct {
	Enabled, Penalties bool
	BurstMs            int
	UDPMaxDelayMs      int
	Default            resolvedRule
	ByEmail            map[string]resolvedRule // ключ: lower(trim(email))
	Profiles           map[string]*profile
}
