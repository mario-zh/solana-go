// Copyright 2020 dfuse Platform Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"reflect"
	"sync"

	"github.com/gorilla/rpc/v2/json2"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

type result interface{}

type WSClient struct {
	rpcURL                  string
	conn                    *websocket.Conn
	lock                    sync.RWMutex
	subscriptionByRequestID map[uint64]*Subscription
	subscriptionByWSSubID   map[uint64]*Subscription
	reconnectOnErr          bool
}

func Dial(ctx context.Context, rpcURL string) (c *WSClient, err error) {
	c = &WSClient{
		rpcURL:                  rpcURL,
		subscriptionByRequestID: map[uint64]*Subscription{},
		subscriptionByWSSubID:   map[uint64]*Subscription{},
	}

	c.conn, _, err = websocket.DefaultDialer.DialContext(ctx, rpcURL, nil)
	if err != nil {
		return nil, fmt.Errorf("new ws client: dial: %w", err)
	}

	go c.receiveMessages()
	return c, nil
}

func (c *WSClient) Close() {
	c.conn.Close()
}

func (c *WSClient) receiveMessages() {
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			c.closeAllSubscription(err)
			return
		}
		c.handleMessage(message)
	}
}

func (c *WSClient) handleMessage(message []byte) {
	// when receiving message with id. the result will be a subscription number.
	// that number will be associated to all future message destine to this request
	if gjson.GetBytes(message, "id").Exists() {
		requestID := uint64(gjson.GetBytes(message, "id").Int())
		subID := uint64(gjson.GetBytes(message, "result").Int())
		c.handleNewSubscriptionMessage(requestID, subID)
		return
	}

	c.handleSubscriptionMessage(uint64(gjson.GetBytes(message, "params.subscription").Int()), message)

}

func (c *WSClient) handleNewSubscriptionMessage(requestID, subID uint64) {
	c.lock.Lock()
	defer c.lock.Unlock()

	zlog.Info("received new subscription message",
		zap.Uint64("message_id", requestID),
		zap.Uint64("subscription_id", subID),
	)
	callBack := c.subscriptionByRequestID[requestID]
	callBack.subID = subID
	c.subscriptionByWSSubID[subID] = callBack
	return
}

func (c *WSClient) handleSubscriptionMessage(subID uint64, message []byte) {
	zlog.Info("received subscription message",
		zap.Uint64("subscription_id", subID),
	)

	c.lock.RLock()
	sub, found := c.subscriptionByWSSubID[subID]
	c.lock.RUnlock()
	if !found {
		zlog.Warn("unbale to find subscription for ws message", zap.Uint64("subscription_id", subID))
		return
	}

	//getting and instantiate the return type for the call back.
	resultType := reflect.New(sub.reflectType)
	result := resultType.Interface()
	err := decodeClientResponse(bytes.NewReader(message), &result)
	if err != nil {
		c.closeSubscription(sub.req.ID, fmt.Errorf("unable to decode client response: %w", err))
		return
	}

	// this cannot be blocking or else
	// we  will no read any other message
	if len(sub.stream) >= cap(sub.stream) {
		c.closeSubscription(sub.req.ID, fmt.Errorf("reached channel max capacity %d", len(sub.stream)))
		return
	}

	sub.stream <- result
	return
}

func (c *WSClient) closeAllSubscription(err error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	for _, sub := range c.subscriptionByRequestID {
		sub.err <- err
	}

	c.subscriptionByRequestID = map[uint64]*Subscription{}
	c.subscriptionByWSSubID = map[uint64]*Subscription{}
}

func (c *WSClient) closeSubscription(reqID uint64, err error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	sub, found := c.subscriptionByRequestID[reqID]
	if !found {
		return
	}

	sub.err <- err

	err = c.rpcUnsubscribe(sub.subID, sub.unsubscriptionMethod)
	if err != nil {
		zlog.Warn("unable to send rpc unsubscribe call",
			zap.Error(err),
		)
	}

	delete(c.subscriptionByRequestID, sub.req.ID)
	delete(c.subscriptionByWSSubID, sub.subID)
}

func (c *WSClient) rpcUnsubscribe(subID uint64, method string) error {
	req := newClientRequest([]interface{}{subID}, method, map[string]interface{}{})
	data, err := req.encode()
	if err != nil {
		return fmt.Errorf("unable to encode unsubscription message for subID %d and method %s", subID, method)
	}

	err = c.conn.WriteMessage(websocket.TextMessage, data)
	if err != nil {
		return fmt.Errorf("unable to send unsubscription message for subID %d and method %s", subID, method)
	}
	return nil
}

type Subscription struct {
	req                  *clientRequest
	subID                uint64
	stream               chan result
	err                  chan error
	reflectType          reflect.Type
	closeFunc            func(err error)
	unsubscriptionMethod string
}

func newSubscription(req *clientRequest, reflectType reflect.Type, closeFunc func(err error)) *Subscription {
	return &Subscription{
		req:         req,
		reflectType: reflectType,
		stream:      make(chan result, 200),
		err:         make(chan error, 1),
		closeFunc:   closeFunc,
	}
}

func (s *Subscription) Recv() (interface{}, error) {
	select {
	case d := <-s.stream:
		return d, nil
	case err := <-s.err:
		return nil, err
	}
}

func (s *Subscription) Unsubscribe() {
	s.unsubscribe(nil)
}

func (s *Subscription) unsubscribe(err error) {
	s.closeFunc(err)

}

func (c *WSClient) ProgramSubscribe(programID string, commitment CommitmentType) (*Subscription, error) {
	return c.subscribe([]interface{}{programID}, "programSubscribe", "programUnsubscribe", commitment, ProgramWSResult{})
}

func (c *WSClient) subscribe(params []interface{}, subscriptionMethod, unsubscriptionMethod string, commitment CommitmentType, resultType interface{}) (*Subscription, error) {
	conf := map[string]interface{}{
		"encoding": "jsonParsed",
	}
	if commitment != "" {
		conf["commitment"] = string(commitment)
	}

	req := newClientRequest(params, subscriptionMethod, conf)
	data, err := req.encode()
	if err != nil {
		return nil, fmt.Errorf("subscribe: unable to encode subsciption request: %w", err)
	}

	sub := newSubscription(req, reflect.TypeOf(resultType), func(err error) {
		c.closeSubscription(req.ID, err)
	})

	c.lock.Lock()
	c.subscriptionByRequestID[req.ID] = sub
	zlog.Info("added new subscription to websocket client", zap.Int("count", len(c.subscriptionByRequestID)))
	c.lock.Unlock()

	err = c.conn.WriteMessage(websocket.TextMessage, data)
	if err != nil {
		return nil, fmt.Errorf("unable to write request: %w", err)
	}

	return sub, nil
}

type ProgramWSResult struct {
	Context struct {
		Slot uint64
	} `json:"context"`
	Value struct {
		Account Account `json:"account"`
	} `json:"value"`
}

type clientRequest struct {
	Version string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	ID      uint64      `json:"id"`
}

func newClientRequest(params []interface{}, method string, configuration map[string]interface{}) *clientRequest {
	params = append(params, configuration)
	return &clientRequest{
		Version: "2.0",
		Method:  method,
		Params:  params,
		ID:      uint64(rand.Int63()),
	}
}

func (c *clientRequest) encode() ([]byte, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("encode request: json marshall: %w", err)
	}
	return data, nil
}

type wsClientResponse struct {
	Version string                  `json:"jsonrpc"`
	Params  *wsClientResponseParams `json:"params"`
	Error   *json.RawMessage        `json:"error"`
}

type wsClientResponseParams struct {
	Result       *json.RawMessage `json:"result"`
	Subscription int              `json:"subscription"`
}

func decodeClientResponse(r io.Reader, reply interface{}) (err error) {
	var c *wsClientResponse
	if err := json.NewDecoder(r).Decode(&c); err != nil {
		return err
	}

	if c.Error != nil {
		jsonErr := &json2.Error{}
		if err := json.Unmarshal(*c.Error, jsonErr); err != nil {
			return &json2.Error{
				Code:    json2.E_SERVER,
				Message: string(*c.Error),
			}
		}
		return jsonErr
	}

	if c.Params == nil {
		return json2.ErrNullResult
	}

	return json.Unmarshal(*c.Params.Result, &reply)
}
