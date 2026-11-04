package speedlimit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const (
	penaltyNone = "none"
	maxFileSize = 8 << 20 // 8 MiB
	maxEmails   = 100_000
)

var profileNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Parse разбирает и проверяет конфиг. Возвращает все ошибки сразу (errors.Join) и список предупреждений.
func Parse(data []byte) (*Config, []string, error) {
	var errs []error
	var warnings []string

	// V01: размер файла
	if len(data) > maxFileSize {
		return nil, nil, fmt.Errorf("config file too large: %d bytes (max %d)", len(data), maxFileSize)
	}

	// V03: дубликаты ключей
	if err := checkDupKeys(data); err != nil {
		return nil, nil, err
	}

	// V01, V02: разбор JSON с DisallowUnknownFields
	var raw rawConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, nil, wrapUnknownField(err)
	}
	if dec.More() {
		return nil, nil, errors.New("invalid json: extra data after config")
	}

	// Значения по умолчанию
	cfg := &Config{
		Enabled:       true,
		Penalties:     true,
		BurstMs:       20,
		UDPMaxDelayMs: 100,
	}
	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
	}
	if raw.Penalties != nil {
		cfg.Penalties = *raw.Penalties
	}
	if raw.BurstMs != nil {
		cfg.BurstMs = *raw.BurstMs
	}
	if raw.UDP != nil && raw.UDP.MaxDelayMs != nil {
		cfg.UDPMaxDelayMs = *raw.UDP.MaxDelayMs
	}

	// V21: burstMs и udp.maxDelayMs
	if cfg.BurstMs < 5 || cfg.BurstMs > 100 {
		errs = append(errs, fmt.Errorf("burstMs must be in [5, 100], got %d", cfg.BurstMs))
	}
	if cfg.UDPMaxDelayMs < 0 || cfg.UDPMaxDelayMs > 500 {
		errs = append(errs, fmt.Errorf("udp.maxDelayMs must be in [0, 500], got %d", cfg.UDPMaxDelayMs))
	}

	// Профили: разобрать и проверить
	cfg.Profiles = make(map[string]*profile)
	for name, rp := range raw.Profiles {
		// V07
		if !profileNameRe.MatchString(name) {
			errs = append(errs, fmt.Errorf("profile name %q: use only A-Za-z0-9_.- (1-64 chars)", name))
			continue
		}
		if name == penaltyNone {
			errs = append(errs, fmt.Errorf("profile name %q is reserved", penaltyNone))
			continue
		}

		p, pErrs, pWarns := parseProfile(name, rp)
		errs = append(errs, pErrs...)
		warnings = append(warnings, pWarns...)
		if p != nil {
			cfg.Profiles[name] = p
		}
	}

	// V04: default обязателен
	if raw.Default == nil {
		return nil, nil, errors.New("default is required")
	}
	if raw.Default.Down == nil {
		errs = append(errs, errors.New("default.down is required"))
	}
	if raw.Default.Up == nil {
		errs = append(errs, errors.New("default.up is required"))
	}
	if len(errs) > 0 {
		return nil, nil, errors.Join(errs...)
	}

	// V05, V08, V26: default
	defaultDownKbps := *raw.Default.Down
	defaultUpKbps := *raw.Default.Up
	if err := validateLimit(defaultDownKbps, "default.down"); err != nil {
		errs = append(errs, err)
	}
	if err := validateLimit(defaultUpKbps, "default.up"); err != nil {
		errs = append(errs, err)
	}

	defaultPenNames, err := parsePenalty(raw.Default.Penalty, [2]string{penaltyNone, penaltyNone}, "default")
	if err != nil {
		errs = append(errs, err)
	} else {
		for i, name := range defaultPenNames {
			if name != penaltyNone && name != "" && cfg.Profiles[name] == nil {
				dir := []string{"down", "up"}[i]
				errs = append(errs, fmt.Errorf("default.penalty.%s: unknown profile %q%s", dir, name, suggestProfile(name, cfg.Profiles)))
			}
		}
	}

	cfg.Default = resolvedRule{
		downKbps: defaultDownKbps,
		upKbps:   defaultUpKbps,
		downProf: pickDir(cfg.Profiles, defaultPenNames[0], false),
		upProf:   pickDir(cfg.Profiles, defaultPenNames[1], true),
	}

	// V23
	if defaultDownKbps > 0 && defaultDownKbps < 1000 {
		warnings = append(warnings, fmt.Sprintf("default.down=%d is very low", defaultDownKbps))
	}
	if defaultUpKbps > 0 && defaultUpKbps < 1000 {
		warnings = append(warnings, fmt.Sprintf("default.up=%d is very low", defaultUpKbps))
	}

	// V24
	if (cfg.Default.downProf != nil || cfg.Default.upProf != nil) && defaultDownKbps == 0 && defaultUpKbps == 0 {
		warnings = append(warnings, "default has penalty profiles but both limits are 0 (unlimited)")
	}

	// Users
	cfg.ByEmail = make(map[string]resolvedRule)
	seenEmails := make(map[string]int) // email -> индекс правила (для V20)

	totalEmails := 0
	for i, ru := range raw.Users {
		path := fmt.Sprintf("users[%d]", i)

		// V19
		if len(ru.Emails) == 0 {
			errs = append(errs, fmt.Errorf("%s.emails: non-empty array required", path))
			continue
		}
		if len(ru.Emails) > 1000 {
			errs = append(errs, fmt.Errorf("%s.emails: max 1000 items", path))
			continue
		}

		// Наследование от default
		downKbps := defaultDownKbps
		upKbps := defaultUpKbps
		if ru.Down != nil {
			downKbps = *ru.Down
		}
		if ru.Up != nil {
			upKbps = *ru.Up
		}

		// V05
		if err := validateLimit(downKbps, path+".down"); err != nil {
			errs = append(errs, err)
		}
		if err := validateLimit(upKbps, path+".up"); err != nil {
			errs = append(errs, err)
		}

		// V08, V26: penalty
		penNames, err := parsePenalty(ru.Penalty, defaultPenNames, path)
		if err != nil {
			errs = append(errs, err)
		} else {
			for j, name := range penNames {
				if name != penaltyNone && name != "" && cfg.Profiles[name] == nil {
					dir := []string{"down", "up"}[j]
					errs = append(errs, fmt.Errorf("%s.penalty.%s: unknown profile %q%s", path, dir, name, suggestProfile(name, cfg.Profiles)))
				}
			}
		}

		rule := resolvedRule{
			downKbps: downKbps,
			upKbps:   upKbps,
			downProf: pickDir(cfg.Profiles, penNames[0], false),
			upProf:   pickDir(cfg.Profiles, penNames[1], true),
		}

		// V27
		if rule.downProf == nil && penNames[0] != penaltyNone && penNames[0] != "" && downKbps > 0 {
			if pr := cfg.Profiles[penNames[0]]; pr != nil && pr.down == nil {
				warnings = append(warnings, fmt.Sprintf("%s: profile %q has no down section, no down penalty", path, penNames[0]))
			}
		}
		if rule.upProf == nil && penNames[1] != penaltyNone && penNames[1] != "" && upKbps > 0 {
			if pr := cfg.Profiles[penNames[1]]; pr != nil && pr.up == nil {
				warnings = append(warnings, fmt.Sprintf("%s: profile %q has no up section, no up penalty", path, penNames[1]))
			}
		}

		for _, email := range ru.Emails {
			// V19: проверка email
			normed := normEmail(email)
			if normed == "" {
				errs = append(errs, fmt.Errorf("%s.emails: empty email after trim", path))
				continue
			}
			if len(normed) > 256 {
				errs = append(errs, fmt.Errorf("%s.emails: %q too long (max 256 chars)", path, normed))
				continue
			}
			for _, r := range normed {
				if unicode.IsControl(r) || unicode.IsSpace(r) {
					errs = append(errs, fmt.Errorf("%s.emails: %q contains control/space chars", path, normed))
					break
				}
			}

			// V20: дубликаты
			if prevIdx, dup := seenEmails[normed]; dup {
				errs = append(errs, fmt.Errorf("%q duplicates users[%d]", email, prevIdx))
			} else {
				seenEmails[normed] = i
				cfg.ByEmail[normed] = rule
				totalEmails++
			}
		}
	}

	// V25
	if totalEmails > maxEmails {
		errs = append(errs, fmt.Errorf("total emails %d exceeds limit %d", totalEmails, maxEmails))
	}

	// Перекрёстные проверки V13–V15 для каждого пользователя и default
	checkUserPenalties := func(path string, rule resolvedRule, downName, upName string) {
		// down
		if rule.downProf != nil {
			for i, s := range rule.downProf.steps {
				effLimit := rule.downKbps
				if i > 0 {
					effLimit = rule.downProf.steps[i-1].limitKbps
				}
				if effLimit > 0 {
					// V13
					if s.aboveKbps >= effLimit {
						errs = append(errs, fmt.Errorf("profile %q down step %d (above=%d) is unreachable for %s (level %d limit %d)", downName, i, s.aboveKbps, path, i, effLimit))
					}
					// V14
					if s.aboveKbps > int64(float64(effLimit)*0.95) {
						warnings = append(warnings, fmt.Sprintf("profile %q down step %d above=%d is >95%% of level %d limit %d", downName, i, s.aboveKbps, i, effLimit))
					}
				}
				// V15
				if rule.downKbps > 0 && rule.downKbps <= s.limitKbps {
					warnings = append(warnings, fmt.Sprintf("profile %q down step %d has no effect for %s (base %d <= %d)", downName, i, path, rule.downKbps, s.limitKbps))
				}
			}
		}
		// up
		if rule.upProf != nil {
			for i, s := range rule.upProf.steps {
				effLimit := rule.upKbps
				if i > 0 {
					effLimit = rule.upProf.steps[i-1].limitKbps
				}
				if effLimit > 0 {
					// V13
					if s.aboveKbps >= effLimit {
						errs = append(errs, fmt.Errorf("profile %q up step %d (above=%d) is unreachable for %s (level %d limit %d)", upName, i, s.aboveKbps, path, i, effLimit))
					}
					// V14
					if s.aboveKbps > int64(float64(effLimit)*0.95) {
						warnings = append(warnings, fmt.Sprintf("profile %q up step %d above=%d is >95%% of level %d limit %d", upName, i, s.aboveKbps, i, effLimit))
					}
				}
				// V15
				if rule.upKbps > 0 && rule.upKbps <= s.limitKbps {
					warnings = append(warnings, fmt.Sprintf("profile %q up step %d has no effect for %s (base %d <= %d)", upName, i, path, rule.upKbps, s.limitKbps))
				}
			}
		}
	}

	checkUserPenalties("default", cfg.Default, defaultPenNames[0], defaultPenNames[1])

	for i, ru := range raw.Users {
		if len(ru.Emails) == 0 {
			continue
		}
		path := fmt.Sprintf("users[%d]", i)
		normed := normEmail(ru.Emails[0])
		if normed == "" {
			continue
		}
		rule, ok := cfg.ByEmail[normed]
		if !ok {
			continue
		}
		penNames, _ := parsePenalty(ru.Penalty, defaultPenNames, path)
		checkUserPenalties(path, rule, penNames[0], penNames[1])
	}

	// V22: неиспользуемые профили
	usedProfiles := make(map[string]bool)
	if cfg.Default.downProf != nil {
		usedProfiles[defaultPenNames[0]] = true
	}
	if cfg.Default.upProf != nil {
		usedProfiles[defaultPenNames[1]] = true
	}
	for i, ru := range raw.Users {
		penNames, _ := parsePenalty(ru.Penalty, defaultPenNames, fmt.Sprintf("users[%d]", i))
		if penNames[0] != penaltyNone && penNames[0] != "" {
			usedProfiles[penNames[0]] = true
		}
		if penNames[1] != penaltyNone && penNames[1] != "" {
			usedProfiles[penNames[1]] = true
		}
	}
	for name := range cfg.Profiles {
		if !usedProfiles[name] {
			warnings = append(warnings, fmt.Sprintf("profile %q is not used", name))
		}
	}

	if len(errs) > 0 {
		return nil, warnings, errors.Join(errs...)
	}
	return cfg, warnings, nil
}

func normEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func validateLimit(kbps int64, path string) error {
	if kbps < 0 || kbps > 100_000_000 {
		return fmt.Errorf("%s=%d: must be in [0, 100000000]", path, kbps)
	}
	if kbps > 0 && kbps < 64 {
		return fmt.Errorf("%s=%d: nonzero limit must be >= 64 kbps (use 0 for unlimited)", path, kbps)
	}
	return nil
}

func parseProfile(name string, rp *rawProfile) (*profile, []error, []string) {
	var errs []error
	var warnings []string

	// V09: краткий или раздельный вид
	hasShort := len(rp.Steps) > 0 || len(rp.Recover) > 0
	hasSplit := rp.Down != nil || rp.Up != nil
	if !hasShort && !hasSplit {
		errs = append(errs, fmt.Errorf("profiles.%s: empty profile (use steps/recover or down/up)", name))
		return nil, errs, warnings
	}
	if hasShort && hasSplit {
		errs = append(errs, fmt.Errorf("profiles.%s: mix of short form (steps/recover) and down/up sections", name))
		return nil, errs, warnings
	}

	p := &profile{name: name}

	if hasShort {
		// Краткий вид: одинаковая секция для обоих направлений
		dp, dErrs, dWarns := parseDirProfile(fmt.Sprintf("profiles.%s", name), rp.Steps, rp.Recover)
		errs = append(errs, dErrs...)
		warnings = append(warnings, dWarns...)
		if dp != nil {
			// Копируем для каждого направления
			upCopy := &dirProfile{
				steps: append([]step(nil), dp.steps...),
				rec:   dp.rec,
			}
			p.down = dp
			p.up = upCopy
		}
	} else {
		// Раздельный вид
		if rp.Down != nil {
			dp, dErrs, dWarns := parseDirProfile(fmt.Sprintf("profiles.%s.down", name), rp.Down.Steps, rp.Down.Recover)
			errs = append(errs, dErrs...)
			warnings = append(warnings, dWarns...)
			p.down = dp
		}
		if rp.Up != nil {
			up, uErrs, uWarns := parseDirProfile(fmt.Sprintf("profiles.%s.up", name), rp.Up.Steps, rp.Up.Recover)
			errs = append(errs, uErrs...)
			warnings = append(warnings, uWarns...)
			p.up = up
		}
	}

	return p, errs, warnings
}

func parseDirProfile(path string, rawSteps []rawStep, rawRec json.RawMessage) (*dirProfile, []error, []string) {
	var errs []error
	var warnings []string

	// V10
	if len(rawSteps) == 0 {
		errs = append(errs, fmt.Errorf("%s.steps: 1..8 items required", path))
		return nil, errs, warnings
	}
	if len(rawSteps) > 8 {
		errs = append(errs, fmt.Errorf("%s.steps: 1..8 items required, got %d", path, len(rawSteps)))
		return nil, errs, warnings
	}

	dp := &dirProfile{}

	for i, rs := range rawSteps {
		spath := fmt.Sprintf("%s.steps[%d]", path, i)

		if rs.Above == nil {
			errs = append(errs, fmt.Errorf("%s.above is required", spath))
			continue
		}
		if rs.For == nil {
			errs = append(errs, fmt.Errorf("%s.for is required", spath))
			continue
		}
		if rs.Limit == nil {
			errs = append(errs, fmt.Errorf("%s.limit is required", spath))
			continue
		}

		// V11
		if *rs.Above <= 0 {
			errs = append(errs, fmt.Errorf("%s.above=%d: must be > 0", spath, *rs.Above))
		}
		if *rs.Limit < 64 {
			errs = append(errs, fmt.Errorf("%s.limit=%d: penalty limit must be >= 64 kbps", spath, *rs.Limit))
		}

		// V06: for
		forDur, err := parseDuration(*rs.For, "for", 10*time.Second, 24*time.Hour)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.for: %w", spath, err))
			continue
		}

		s := step{
			aboveKbps: *rs.Above,
			limitKbps: *rs.Limit,
			forTicks:  int(forDur.Seconds()),
		}
		dp.steps = append(dp.steps, s)
	}

	// V12: limit строго убывает
	for i := 1; i < len(dp.steps); i++ {
		if dp.steps[i].limitKbps >= dp.steps[i-1].limitKbps {
			errs = append(errs, fmt.Errorf("%s.steps[%d].limit(%d) must be < steps[%d].limit(%d)", path, i, dp.steps[i].limitKbps, i-1, dp.steps[i-1].limitKbps))
		}
	}

	// V16: recover обязателен
	if len(rawRec) == 0 || string(rawRec) == "null" {
		errs = append(errs, fmt.Errorf("%s.recover is required (use \"never\" to disable explicitly)", path))
		return dp, errs, warnings
	}

	// Проверяем "never"
	var neverStr string
	if err := json.Unmarshal(rawRec, &neverStr); err == nil {
		if neverStr == "never" {
			dp.rec.never = true
			return dp, errs, warnings
		}
		errs = append(errs, fmt.Errorf("%s.recover: unknown string %q (use \"never\" or an object)", path, neverStr))
		return dp, errs, warnings
	}

	// Разбираем объект
	var rr rawRecover
	if err := json.Unmarshal(rawRec, &rr); err != nil {
		errs = append(errs, fmt.Errorf("%s.recover: %w", path, err))
		return dp, errs, warnings
	}

	// V16: непустой объект
	if rr.Below == nil && rr.For == nil && rr.After == nil {
		errs = append(errs, fmt.Errorf("%s.recover: empty object (use at least one of below/for/after or \"never\")", path))
		return dp, errs, warnings
	}

	// V16: below требует for
	if (rr.Below != nil) != (rr.For != nil) {
		errs = append(errs, fmt.Errorf("%s.recover: below and for must be used together", path))
	}

	if rr.Below != nil && rr.For != nil {
		// V16: below > 0
		if *rr.Below <= 0 {
			errs = append(errs, fmt.Errorf("%s.recover.below=%d: must be > 0", path, *rr.Below))
		}
		dp.rec.belowKbps = *rr.Below

		// V06: for
		forDur, err := parseDuration(*rr.For, "for", 10*time.Second, 24*time.Hour)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.recover.for: %w", path, err))
		} else {
			dp.rec.forTicks = int(forDur.Seconds())
		}
	}

	if rr.After != nil {
		// V06: after
		afterDur, err := parseDuration(*rr.After, "after", time.Minute, 720*time.Hour)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.recover.after: %w", path, err))
		} else {
			dp.rec.afterTicks = int(afterDur.Seconds())
		}
	}

	// V17: below < min(step.above)
	if dp.rec.belowKbps > 0 {
		minAbove := int64(1<<63 - 1)
		for _, s := range dp.steps {
			if s.aboveKbps < minAbove {
				minAbove = s.aboveKbps
			}
		}
		if dp.rec.belowKbps >= minAbove {
			errs = append(errs, fmt.Errorf("%s.recover.below(%d) must be < min(step.above)=%d", path, dp.rec.belowKbps, minAbove))
		}
	}

	// V18: below < каждого step.limit
	if dp.rec.belowKbps > 0 {
		for i, s := range dp.steps {
			if dp.rec.belowKbps >= s.limitKbps {
				errs = append(errs, fmt.Errorf("%s.recover.below(%d) >= steps[%d].limit(%d): use recover.after for a fixed-duration penalty", path, dp.rec.belowKbps, i, s.limitKbps))
			}
		}
	}

	return dp, errs, warnings
}

func parseDuration(s, field string, min, max time.Duration) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("use a string like \"5m\": %w", err)
	}
	if d < min || d > max {
		return 0, fmt.Errorf("must be in [%v, %v], got %v", min, max, d)
	}
	return d, nil
}

// parsePenalty возвращает имена профилей [down, up]. inherit — значения, которые
// остаются, если поле или ключ опущены (для users это имена из default).
func parsePenalty(raw json.RawMessage, inherit [2]string, path string) ([2]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return inherit, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return [2]string{s, s}, nil // строка = оба направления
	}
	var o struct {
		Down *string `json:"down"`
		Up   *string `json:"up"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields() // V26
	if err := dec.Decode(&o); err != nil {
		return inherit, fmt.Errorf("%s.penalty: use a profile name or {\"down\":..., \"up\":...}: %w", path, err)
	}
	out := inherit
	if o.Down != nil {
		out[0] = *o.Down
	}
	if o.Up != nil {
		out[1] = *o.Up
	}
	return out, nil
}

// pickDir возвращает секцию направления профиля name; nil для "none"/пустого имени
// или если у профиля нет секции этого направления (последнее даёт предупреждение V27).
func pickDir(profiles map[string]*profile, name string, up bool) *dirProfile {
	if name == penaltyNone || name == "" {
		return nil
	}
	pr := profiles[name]
	if pr == nil {
		return nil // имя уже проверено по V08; сюда не попадаем
	}
	if up {
		return pr.up
	}
	return pr.down
}

func checkDupKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	return walkKeys(dec, "$")
}

func walkKeys(dec *json.Decoder, path string) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			k := kt.(string)
			if _, dup := seen[k]; dup {
				return fmt.Errorf("%s: duplicate key %q", path, k)
			}
			seen[k] = struct{}{}
			if err := walkKeys(dec, path+"."+k); err != nil {
				return err
			}
		}
		_, err = dec.Token() // '}'
		return err
	case '[':
		for i := 0; dec.More(); i++ {
			if err := walkKeys(dec, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		_, err = dec.Token() // ']'
		return err
	}
	return nil
}

func wrapUnknownField(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "unknown field") {
		// Попытка извлечь имя неизвестного поля и подсказать ближайшее
		// Простая реализация без полного разбора
		return err
	}
	return err
}

func suggestProfile(name string, profiles map[string]*profile) string {
	if len(profiles) == 0 {
		return ""
	}
	// Простейшая реализация: находим ближайшее по расстоянию Левенштейна
	best := ""
	bestDist := 1000
	for pname := range profiles {
		dist := levenshtein(name, pname)
		if dist < bestDist && dist <= 2 {
			bestDist = dist
			best = pname
		}
	}
	if best != "" {
		return fmt.Sprintf(" (did you mean %q?)", best)
	}
	return ""
}

func levenshtein(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	prev := make([]int, len(b)+1)
	for i := range prev {
		prev[i] = i
	}
	for i := 0; i < len(a); i++ {
		curr := make([]int, len(b)+1)
		curr[0] = i + 1
		for j := 0; j < len(b); j++ {
			cost := 0
			if a[i] != b[j] {
				cost = 1
			}
			curr[j+1] = min(curr[j]+1, min(prev[j+1]+1, prev[j]+cost))
		}
		prev = curr
	}
	return prev[len(b)]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
