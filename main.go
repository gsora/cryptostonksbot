package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/leekchan/accounting"
	"github.com/nleeper/goment"
	gecko "github.com/superoo7/go-gecko/v3"
	"github.com/superoo7/go-gecko/v3/types"

	tb "gopkg.in/tucnak/telebot.v2"
)

var coinInfoFmt = `{{.Name}} ({{.Symbol}})

Price: {{.Price}} ({{.PriceChangePercent}} {{.PriceEmoji}})
Market cap: {{.MarketCap}}({{.MarketCapPercent}} {{.MarketCapEmoji}})
ATH: {{.ATH}} ({{.ATHDate}}) 🌝
Last 24H high: {{.High24}}
Last 24H low: {{.Low24}}
`

var (
	errInvalidCurrency = cgErr{Value: "invalid vs_currency"}

	defaultSticker = &tb.Sticker{
		File: tb.File{
			FileID:   "CAACAgQAAxkBAAEI-mpgKkXH3BZf3aD5e5Nltp9GmAfbKgACIQADFqBBJiaiOzMAAViZ0R4E",
			UniqueID: "AgADIQADFqBBJg",
		},
		Width:    512,
		Height:   512,
		Animated: true,
	}
)

const (
	defaultCurrency     = "USD"
	defaultUpdateTicker = 1 * time.Second
	tgTokenEnv          = "CSB_TOKEN"
)

func doIt() bool {
	var v uint64
	err := binary.Read(rand.Reader, binary.BigEndian, &v)
	if err != nil {
		return false
	}

	if (v%5 == 0) && (v%3 == 0) {
		return true
	}

	return false
}

type cgErr struct {
	Value string `json:"error,omitempty"`
}

func (cg cgErr) Error() string {
	return cg.Value
}

type coinQuerier struct {
	gc            *gecko.Client
	idMap         map[string]string
	idMapUpdating sync.Mutex
}

func (cq *coinQuerier) loadCoinIDs() error {
	cq.idMapUpdating.Lock()
	defer cq.idMapUpdating.Unlock()

	l, err := cq.gc.CoinsList()
	if err != nil {
		return err
	}

	nim := make(map[string]string)
	for _, e := range *l {
		nim[e.Symbol] = e.ID
	}

	cq.idMap = nim

	return nil
}

func (cq *coinQuerier) backgroundIDUpdates() {
	if err := cq.loadCoinIDs(); err != nil {
		log.Println("background id update error:", err)
	}

	for range time.NewTicker(defaultUpdateTicker).C {
		if err := cq.loadCoinIDs(); err != nil {
			log.Println("background id update error:", err)
		}
	}
}

func (cq *coinQuerier) lookupPrice(token, currency string) (types.CoinsMarketItem, error) {
	cq.idMapUpdating.Lock()
	defer cq.idMapUpdating.Unlock()

	id, found := cq.idMap[strings.ToLower(token)]
	if !found {
		return types.CoinsMarketItem{}, fmt.Errorf("%s not supported", token)
	}

	r, err := cq.gc.CoinsMarket(currency, []string{id}, "market_cap_desc", 250, 1, false, []string{"24h"})
	if err != nil {
		var v cgErr
		if err := json.Unmarshal([]byte(err.Error()), &v); err != nil {
			return types.CoinsMarketItem{}, err
		}

		if v == errInvalidCurrency {
			return types.CoinsMarketItem{}, fmt.Errorf("%s not supported", strings.ToUpper(currency))
		}

		return types.CoinsMarketItem{}, v
	}

	if len(*r) == 0 {
		return types.CoinsMarketItem{}, fmt.Errorf("no results found")
	}

	return (*r)[0], nil
}

type templateData struct {
	types.CoinsMarketItem
	Price              string
	PriceChangePercent string
	MarketCap          string
	MarketCapPercent   string
	High24             string
	Low24              string
	ATH                string
	ATHDate            string
}

func (t *templateData) format(currency string) {
	lc := accounting.LocaleInfo[strings.ToUpper(currency)]
	a := accounting.Accounting{Symbol: lc.ComSymbol, Precision: 2, Thousand: lc.ThouSep, Decimal: lc.DecSep}
	t.Symbol = "$" + strings.ToUpper(t.Symbol)

	t.Price = a.FormatMoneyFloat64(t.CoinsMarketItem.CurrentPrice)
	t.PriceChangePercent = fmt.Sprintf("%.2f%%", t.PriceChangePercentage24h)
	t.MarketCap = a.FormatMoneyFloat64(t.CoinsMarketItem.MarketCap)
	t.MarketCapPercent = fmt.Sprintf("%.2f%%", t.MarketCapChangePercentage24h)
	t.High24 = a.FormatMoneyFloat64(t.CoinsMarketItem.High24)
	t.Low24 = a.FormatMoneyFloat64(t.CoinsMarketItem.Low24)
	t.ATH = a.FormatMoneyFloat64(t.CoinsMarketItem.ATH)

	pt, err := time.Parse(time.RFC3339, t.CoinsMarketItem.ATHDate)
	if err != nil {
		panic(err)
	}
	tt, err := goment.New(pt)
	if err != nil {
		panic(err)
	}

	t.ATHDate = tt.Format("D MMMM YYYY")
}

func (t templateData) PriceEmoji() string {
	if t.PriceChange24h > 0 {
		return "🤑"
	}

	return "🤮"
}

func (t templateData) MarketCapEmoji() string {
	if t.PriceChange24h > 0 {
		return "🤑"
	}

	return "🤮"
}

func (cq *coinQuerier) queryMessage(ticker, fiatCurrency string) string {
	ticker = strings.ToUpper(ticker)[1:]
	p, err := cq.lookupPrice(ticker, fiatCurrency)
	if err != nil {
		return err.Error()
	}

	t := templateData{CoinsMarketItem: p}
	t.format(fiatCurrency)
	tt, err := template.New("t").Parse(coinInfoFmt)
	if err != nil {
		// cannot generate template somehow
		log.Println("cannot generate message template:", err)
		return "Cannot query CoinGecko API"
	}

	out := &strings.Builder{}
	if err := tt.Execute(out, t); err != nil {
		// cannot execute template somehow
		log.Println("cannot execute message template:", err)
		return "Cannot query CoinGecko API"
	}

	return out.String()
}

func main() {
	token := os.Getenv(tgTokenEnv)
	if token == "" {
		panic("missing telegram bot token in " + tgTokenEnv + " env var")
	}

	hc := &http.Client{
		Timeout: 10 * time.Second,
	}

	cq := coinQuerier{gc: gecko.NewClient(hc)}

	go cq.backgroundIDUpdates()

	b, err := tb.NewBot(tb.Settings{
		Token: token,
		Poller: &tb.LongPoller{
			Timeout:        10 * time.Second,
			AllowedUpdates: []string{"message"},
		},
	})

	if err != nil {
		log.Fatal(err)
		return
	}

	b.Handle(tb.OnText, func(m *tb.Message) {
		if !strings.HasPrefix(m.Text, "%") {
			return
		}

		ticker, currency, err := queryData(m.Text)
		if err != nil {
			log.Println("cannot split query data:", err)
			return
		}

		b.Send(m.Chat, cq.queryMessage(ticker, currency))

		if doIt() {
			b.Send(m.Chat, defaultSticker)
		}
	})

	b.Start()
}

func queryData(s string) (ticker, currency string, err error) {
	currency = defaultCurrency

	f := strings.Fields(s)
	switch len(f) {
	case 1:
		ticker = f[0]
		return
	case 2:
		ticker = f[0]
		currency = f[1]
		return
	default:
		err = fmt.Errorf("no fields available")
		return
	}
}
