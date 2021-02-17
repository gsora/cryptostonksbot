package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
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

var (
	coinInfoFmt = `{{.Name}} ({{.Symbol}})

Price: {{.Price}} ({{.PriceChangePercent}} {{.PriceEmoji}})
Market cap: {{.MarketCap}}({{.MarketCapPercent}} {{.MarketCapEmoji}})
ATH: {{.ATH}} ({{.ATHDate}}) 🌝
Last 24H high: {{.High24}}
Last 24H low: {{.Low24}}
`

	ifIHadFmt          = "%s worth of %s in %d (priced at %s) are now worth %s 😱"
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

func (cq *coinQuerier) coinInfo(token string) (*types.CoinsID, error) {
	id, found := cq.idMap[strings.ToLower(token)]
	if !found {
		return nil, fmt.Errorf("%s not supported", token)
	}

	return cq.gc.CoinsID(id, false, false, false, false, false, false)
}

func (cq *coinQuerier) ifIBought(token, currency string, year, month int, amount float64) (float64, float64, float64, error) {
	currency = strings.ToLower(currency)

	id, found := cq.idMap[strings.ToLower(token)]
	if !found {
		return 0, 0, 0, fmt.Errorf("%s not supported", token)
	}

	if month <= 0 || month > 12 {
		month = int(time.Now().Month())
	}

	dateStr := fmt.Sprintf("15-%d-%d", month, year)

	data, err := cq.gc.CoinsIDHistory(id, dateStr, false)
	log.Println(dateStr)
	if err != nil {
		ret := fmt.Errorf("%s not supported", token)
		if errors.Is(err, context.DeadlineExceeded) {
			ret = fmt.Errorf("CoinGecko API connection failed 🤕")
		}
		log.Println("cannot query history data:", err)
		return 0, 0, 0, ret
	}

	if data.MarketData == nil {
		return 0, 0, 0, fmt.Errorf("No data available for year %d", year)
	}

	oldPrice, ok := data.MarketData.CurrentPrice[currency]
	if !ok {
		return 0, 0, 0, fmt.Errorf("%s not supported", currency)
	}

	newData, err := cq.lookupPrice(token, currency)
	if err != nil {
		return 0, 0, 0, err
	}

	oldAmount := amount / oldPrice
	newValue := oldAmount * newData.CurrentPrice

	return oldAmount, newValue, oldPrice, nil
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
			AllowedUpdates: []string{"message", "inline_query", "chosen_inline_result"},
		},
		Verbose: false,
	})

	if err != nil {
		log.Fatal(err)
		return
	}

	b.Handle(tb.OnText, func(m *tb.Message) {
		if !strings.HasPrefix(m.Text, "%") {
			return
		}

		ticker := strings.ToUpper(m.Text)[1:]

		ticker, currency, err := queryData(ticker)
		if err != nil {
			log.Println("cannot split query data:", err)
			return
		}

		b.Send(m.Chat, cq.queryMessage(ticker, currency))

		if doIt() {
			b.Send(m.Chat, defaultSticker)
		}
	})

	b.Handle(tb.OnQuery, func(q *tb.Query) {
		rawData := strings.Split(q.Text, " ")
		if len(rawData) == 0 {
			return
		}

		tokenID := strings.TrimSpace(rawData[0])
		if len(tokenID) == 0 {
			return
		}

		data, err := cq.coinInfo(tokenID)
		if err != nil {
			log.Println("cannot query inline token infos:", err)
			return
		}

		result := &tb.ArticleResult{
			Title:       fmt.Sprintf("%s (%s) informations", data.Name, tokenID),
			Text:        cq.queryMessage(tokenID, defaultCurrency),
			Description: fmt.Sprintf("Get current price, market cap infos about %s!", data.Name),
			ThumbURL:    data.Image.Large,
		}

		result.SetResultID("0")

		err = b.Answer(q, &tb.QueryResponse{
			Results:   tb.Results{result},
			CacheTime: 1, // a minute
		})

		if err != nil {
			log.Println(err)
		}
	})

	b.Handle("/whatif", func(m *tb.Message) {
		f := strings.Fields(m.Text)
		f = f[1:]

		// /whatif 450usd btc
		if len(f) < 3 {
			b.Send(m.Chat, "Syntax: /whatif amount token year [month number]")
			return
		}

		amount, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			b.Send(m.Chat, "Malformed amount")
			return
		}

		token := f[1]

		year, err := strconv.Atoi(f[2])
		if err != nil {
			b.Send(m.Chat, "Malformed year")
			return
		}

		var month int
		if len(f) == 4 {
			mm, err := strconv.Atoi(f[2])
			if err != nil {
				b.Send(m.Chat, "Malformed month")
			}

			month = mm
		}

		_, new, oldPrice, err := cq.ifIBought(token, defaultCurrency, year, month, amount)
		if err != nil {
			b.Send(m.Chat, err.Error())
			return
		}

		lc := accounting.LocaleInfo[strings.ToUpper(defaultCurrency)]
		a := accounting.Accounting{Symbol: lc.ComSymbol, Precision: 2, Thousand: lc.ThouSep, Decimal: lc.DecSep}

		ss := fmt.Sprintf(ifIHadFmt,
			a.FormatMoneyInt(500),
			strings.ToUpper(token),
			year,
			a.FormatMoneyFloat64(oldPrice),
			a.FormatMoneyFloat64(new),
		)
		b.Send(m.Chat, ss)

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
