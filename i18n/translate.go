// i18n/translate.go — Tipovačka 3.0
// Překlad dynamického obsahu (názvy soutěží, extra otázky) CS → EN
// přes Google Translate free API, s in-memory cache.
package i18n

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	dynCache   = map[string]string{}
	dynCacheMu sync.RWMutex
	dynClient  = &http.Client{Timeout: 5 * time.Second}
)

// TrDynamic přeloží libovolný text do angličtiny, pokud lang == "en".
// Pro češtinu (nebo prázdný text) vrací originál. Výsledky se cachují,
// při chybě API vrací originál.
func TrDynamic(lang, text string) string {
	if lang != "en" || strings.TrimSpace(text) == "" {
		return text
	}

	dynCacheMu.RLock()
	if v, ok := dynCache[text]; ok {
		dynCacheMu.RUnlock()
		return v
	}
	dynCacheMu.RUnlock()

	translated := googleTranslateEN(text)
	if translated == "" {
		translated = text
	}

	dynCacheMu.Lock()
	dynCache[text] = translated
	dynCacheMu.Unlock()
	return translated
}

// googleTranslateEN přeloží text z češtiny do angličtiny přes Google Translate (free API).
func googleTranslateEN(text string) string {
	apiURL := "https://translate.googleapis.com/translate_a/single?client=gtx&sl=cs&tl=en&dt=t&q=" + url.QueryEscape(text)
	resp, err := dynClient.Get(apiURL)
	if err != nil {
		log.Printf("[i18n/translate] chyba: %v", err)
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Odpověď: [[[přeloženýText, originál, ...], ...], null, "cs"]
	var rawAny []interface{}
	if err := json.Unmarshal(body, &rawAny); err != nil || len(rawAny) == 0 {
		return ""
	}
	parts, ok := rawAny[0].([]interface{})
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if arr, ok := p.([]interface{}); ok && len(arr) > 0 {
			if s, ok := arr[0].(string); ok {
				sb.WriteString(s)
			}
		}
	}
	return sb.String()
}
