package bbgo

import (
	"context"
	"fmt"
	"github.com/adshao/go-binance"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"strconv"
	"strings"
	"time"
)

type SubscribeOptions struct {
	Interval string
	Depth    string
}

func (o SubscribeOptions) String() string {
	if len(o.Interval) > 0 {
		return o.Interval
	}

	return o.Depth
}

type Subscription struct {
	Symbol  string
	Channel string
	Options SubscribeOptions
}

func (s *Subscription) String() string {
	return fmt.Sprintf("%s@%s_%s", strings.ToLower(s.Symbol), s.Channel, s.Options.String())
}

type StreamRequest struct {
	// request ID is required
	ID     int      `json:"id"`
	Method string   `json:"method"`
	Params []string `json:"params"`
}

type PrivateStream struct {
	Client        *binance.Client
	ListenKey     string
	Conn          *websocket.Conn
	Subscriptions []Subscription
}

func (s *PrivateStream) Subscribe(channel string, symbol string, options SubscribeOptions) {
	s.Subscriptions = append(s.Subscriptions, Subscription{
		Channel: channel,
		Symbol:  symbol,
		Options: options,
	})
}

func (s *PrivateStream) Connect(ctx context.Context) error {
	url := "wss://stream.binance.com:9443/ws/" + s.ListenKey
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return err
	}

	log.Infof("[binance] websocket connected")
	s.Conn = conn

	var params []string
	for _, subscription := range s.Subscriptions {
		params = append(params, subscription.String())
	}

	log.Infof("[binance] subscribing channels: %+v", params)
	err = conn.WriteJSON(StreamRequest{
		Method: "SUBSCRIBE",
		Params: params,
		ID:     1,
	})

	return err
}

func (s *PrivateStream) Read(ctx context.Context, eventC chan interface{}) {
	defer close(eventC)

	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for {
		select {

		case <-ctx.Done():
			return

		case <-ticker.C:
			err := s.Client.NewKeepaliveUserStreamService().ListenKey(s.ListenKey).Do(ctx)
			if err != nil {
				log.WithError(err).Error("listen key keep-alive error", err)
			}

		default:
			if err := s.Conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
				log.WithError(err).Errorf("set read deadline error: %s", err.Error())
			}

			mt, message, err := s.Conn.ReadMessage()
			if err != nil {
				log.WithError(err).Errorf("read error: %s", err.Error())
				return
			}

			// skip non-text messages
			if mt != websocket.TextMessage {
				continue
			}

			log.Debugf("[binance] recv: %s", message)

			e, err := parseEvent(string(message))
			if err != nil {
				log.WithError(err).Errorf("[binance] event parse error")
				continue
			}

			eventC <- e
		}
	}
}

func (s *PrivateStream) Close() error {
	log.Infof("[binance] closing user data stream...")

	defer s.Conn.Close()

	// use background context to close user stream
	err := s.Client.NewCloseUserStreamService().ListenKey(s.ListenKey).Do(context.Background())
	if err != nil {
		log.WithError(err).Error("[binance] error close user data stream")
		return err
	}

	return err
}

type BinanceExchange struct {
	Client *binance.Client
}

func (e *BinanceExchange) NewPrivateStream(ctx context.Context) (*PrivateStream, error) {
	log.Infof("[binance] creating user data stream...")
	listenKey, err := e.Client.NewStartUserStreamService().Do(ctx)
	if err != nil {
		return nil, err
	}

	log.Infof("[binance] user data stream created. listenKey: %s", listenKey)
	return &PrivateStream{
		Client:    e.Client,
		ListenKey: listenKey,
	}, nil
}

func (e *BinanceExchange) SubmitOrder(ctx context.Context, order *Order) error {
	/*
		limit order example

			order, err := Client.NewCreateOrderService().
				Symbol(Symbol).
				Side(side).
				Type(binance.OrderTypeLimit).
				TimeInForce(binance.TimeInForceTypeGTC).
				Quantity(volumeString).
				Price(priceString).
				Do(ctx)
	*/

	req := e.Client.NewCreateOrderService().
		Symbol(order.Symbol).
		Side(order.Side).
		Type(order.Type).
		Quantity(order.VolumeStr)

	if len(order.PriceStr) > 0 {
		req.Price(order.PriceStr)
	}
	if len(order.TimeInForce) > 0 {
		req.TimeInForce(order.TimeInForce)
	}

	retOrder, err := req.Do(ctx)
	log.Infof("order created: %+v", retOrder)
	return err
}

func (e *BinanceExchange) QueryKLines(ctx context.Context, symbol, interval string, limit int) ([]KLine, error) {
	resp, err := e.Client.NewKlinesService().Symbol(symbol).Interval(interval).Limit(limit).Do(ctx)
	if err != nil {
		return nil, err
	}

	var kLines []KLine
	for _, kline := range resp {
		kLines = append(kLines, KLine{
			Symbol:         symbol,
			Interval:       interval,
			StartTime:      kline.OpenTime,
			EndTime:        kline.CloseTime,
			Open:           kline.Open,
			Close:          kline.Close,
			High:           kline.High,
			Low:            kline.Low,
			Volume:         kline.Volume,
			QuoteVolume:    kline.QuoteAssetVolume,
			NumberOfTrades: kline.TradeNum,
		})
	}
	return kLines, nil
}

func (e *BinanceExchange) QueryTrades(ctx context.Context, market string, startTime time.Time) (trades []Trade, err error) {
	var lastTradeID int64 = 0
	for {
		req := e.Client.NewListTradesService().
			Limit(1000).
			Symbol(market).
			StartTime(startTime.UnixNano() / 1000000)

		if lastTradeID > 0 {
			req.FromID(lastTradeID)
		}

		bnTrades, err := req.Do(ctx)
		if err != nil {
			return nil, err
		}

		if len(bnTrades) <= 1 {
			break
		}

		for _, t := range bnTrades {
			// skip trade ID that is the same
			if t.ID == lastTradeID {
				continue
			}

			var side string
			if t.IsBuyer {
				side = "BUY"
			} else {
				side = "SELL"
			}

			// trade time
			tt := time.Unix(0, t.Time*1000000)

			log.Infof("trade: %d %4s Price: % 13s Volume: % 13s %s", t.ID, side, t.Price, t.Quantity, tt)

			price, err := strconv.ParseFloat(t.Price, 64)
			if err != nil {
				return nil, err
			}

			quantity, err := strconv.ParseFloat(t.Quantity, 64)
			if err != nil {
				return nil, err
			}

			fee, err := strconv.ParseFloat(t.Commission, 64)
			if err != nil {
				return nil, err
			}

			trades = append(trades, Trade{
				ID:          t.ID,
				Price:       price,
				Volume:      quantity,
				IsBuyer:     t.IsBuyer,
				IsMaker:     t.IsMaker,
				Fee:         fee,
				FeeCurrency: t.CommissionAsset,
				Time:        tt,
			})

			lastTradeID = t.ID
		}
	}

	return trades, nil
}