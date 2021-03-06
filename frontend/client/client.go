package client

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/alpacahq/marketstore/frontend"
	"github.com/alpacahq/marketstore/frontend/stream"
	"github.com/alpacahq/marketstore/utils/io"
	"github.com/alpacahq/marketstore/utils/rpc/msgpack2"
	"github.com/golang/glog"
	"github.com/gorilla/websocket"
	"github.com/vmihailenco/msgpack"
)

type Client struct {
	BaseURL string
}

func NewClient(baseurl string) (cl *Client, err error) {
	cl = new(Client)
	_, err = url.Parse(baseurl)
	if err != nil {
		return nil, err
	}
	cl.BaseURL = baseurl
	return cl, nil
}

func (cl *Client) DoRPC(functionName string, args interface{}) (csm io.ColumnSeriesMap, err error) {
	/*
		Does a remote procedure call using the msgpack2 protocol for RPC that return a QueryReply
	*/
	if args == nil {
		return nil, fmt.Errorf("args must be non-nil - have: args: %v\n",
			args)
	}
	message, err := msgpack2.EncodeClientRequest("DataService."+functionName, args)
	if err != nil {
		return nil, err
	}
	reqURL := cl.BaseURL + "/rpc"
	req, err := http.NewRequest("POST", reqURL, bytes.NewBuffer(message))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-msgpack")
	client := new(http.Client)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		var errText string
		if err != nil {
			errText = err.Error()
		} else {
			if bodyBytes != nil {
				errText = string(bodyBytes)
			}
		}
		return nil, fmt.Errorf("response error (%d): %s", resp.StatusCode, errText)
	}

	switch functionName {
	case "Query", "SQLStatement":
		result := &frontend.MultiQueryResponse{}
		err = msgpack2.DecodeClientResponse(resp.Body, result)
		if err != nil {
			fmt.Printf("Error decoding: %s\n", err)
			return nil, err
		}
		return ConvertMultiQueryReplyToColumnSeries(result)
	case "Write":
		result := &frontend.MultiWriteResponse{}
		err = msgpack2.DecodeClientResponse(resp.Body, result)
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported RPC response")
	}
}

func ConvertMultiQueryReplyToColumnSeries(result *frontend.MultiQueryResponse) (csm io.ColumnSeriesMap, err error) {
	if result == nil {
		return nil, nil
	}
	csm = io.NewColumnSeriesMap()
	for _, ds := range result.Responses { // Datasets are packed in a slice, each has a NumpyMultiDataset inside
		nmds := ds.Result
		for tbkStr, startIndex := range nmds.StartIndex {
			cs, err := nmds.ToColumnSeries(startIndex, nmds.Lengths[tbkStr])
			if err != nil {
				return nil, err
			}
			tbk := io.NewTimeBucketKeyFromString(tbkStr)
			csm[*tbk] = cs
		}
	}
	return csm, nil
}

func ColumnSeriesFromResult(shapes []io.DataShape, columns map[string]interface{}) (cs *io.ColumnSeries, err error) {
	cs = io.NewColumnSeries()
	for _, shape := range shapes {
		name := shape.Name
		typ := shape.Type
		base := columns[name].([]interface{})
		if base == nil {
			return nil, fmt.Errorf("unable to unpack %s", name)
		}
		i_column, err := io.CreateSliceFromSliceOfInterface(base, typ)
		if err != nil {
			return nil, err
		}
		cs.AddColumn(name, i_column)
	}
	return cs, nil
}

// Subscribe to the marketstore websocket interface with a
// message handler, a set of streams and cancel channel.
func (cl *Client) Subscribe(
	handler func(pl stream.Payload) error,
	cancel <-chan struct{},
	streams ...string) (done <-chan struct{}, err error) {

	u, _ := url.Parse(cl.BaseURL + "/ws")
	u.Scheme = "ws"

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)

	if err != nil {
		return nil, err
	}

	buf, err := msgpack.Marshal(stream.SubscribeMessage{Streams: streams})
	if err != nil {
		return nil, err
	}

	if err := conn.WriteMessage(websocket.BinaryMessage, buf); err != nil {
		return nil, err
	}

	select {
	case buf = <-read(conn, make(chan struct{}), 1):
		// make sure subscription succeeded
		subRespMsg := &stream.SubscribeMessage{}
		if err = msgpack.Unmarshal(buf, subRespMsg); err != nil {
			conn.Close()
			return nil, fmt.Errorf("marketstore stream subscribe failed (%s)", err)
		}
		if !streamsEqual(streams, subRespMsg.Streams) {
			conn.Close()
			return nil, fmt.Errorf("marketstore stream subscribe failed")
		}
	case <-time.After(10 * time.Second):
		// timeout
		conn.Close()
		return nil, fmt.Errorf("marketstore stream subscribe timed out")
	}

	return streamConn(conn, handler, cancel), nil
}

func streamConn(
	c *websocket.Conn,
	handler func(pl stream.Payload) error,
	cancel <-chan struct{}) <-chan struct{} {

	done := make(chan struct{}, 1)

	go func() {
		defer c.Close()
		bufC := read(c, done, -1)

		for {
			finished := false

			select {
			case buf, ok := <-bufC:
				if ok {
					pl := stream.Payload{}

					// convert to payload
					if err := msgpack.Unmarshal(buf, &pl); err != nil {
						glog.Errorf("error unmarshaling stream message (%v)", err)
						continue
					}

					// handle payload
					if err := handler(pl); err != nil {
						glog.Errorf("error handling stream message (%v)", err)
						continue
					}
				} else {
					finished = true
				}
			case <-cancel:
				finished = true
			}
			if finished {
				break
			}
		}
	}()

	return done
}

func read(c *websocket.Conn, done chan struct{}, count int) chan []byte {
	bufC := make(chan []byte, 1)
	msgsRead := 0
	go func() {
		defer close(bufC)
		for {
			msgType, buf, err := c.ReadMessage()

			if err != nil {
				if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					glog.Errorf("unexpected websocket closure (%v)", err)
				}
				done <- struct{}{}
				return
			}

			switch msgType {
			case websocket.PingMessage:
				err = c.WriteMessage(websocket.PongMessage, []byte{})
			case websocket.PongMessage:
				err = c.WriteMessage(websocket.PingMessage, []byte{})
			case websocket.TextMessage:
				fallthrough
			case websocket.BinaryMessage:
				bufC <- buf
			case websocket.CloseMessage:
				return
			}

			msgsRead++
			if count > 0 && msgsRead >= count {
				break
			}
		}
	}()

	return bufC
}

func streamsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i, v := range a {
		if !strings.EqualFold(v, b[i]) {
			return false
		}
	}
	return true
}
