// Package http provides a http client
package http

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/micro/go-micro/broker"
	"github.com/micro/go-micro/client"
	"github.com/micro/go-micro/cmd"
	"github.com/micro/go-micro/codec"
	"github.com/micro/go-micro/errors"
	"github.com/micro/go-micro/metadata"
	"github.com/micro/go-micro/registry"
	"github.com/micro/go-micro/selector"
	"github.com/micro/go-micro/transport"
)

type httpClient struct {
	once sync.Once
	opts client.Options
}

func init() {
	cmd.DefaultClients["http"] = NewClient
}

func (h *httpClient) next(request client.Request, opts client.CallOptions) (selector.Next, error) {
	// return remote address
	if len(opts.Address) > 0 && opts.Address[:1] != ":" {
		return func() (*registry.Node, error) {
			return &registry.Node{
				Address: opts.Address,
			}, nil
		}, nil
	}

	// TODO service address
	addr := strings.Replace(request.Service(), ".", "-", -1)

	// TODO Opts FQDN
	// ns := os.Getenv("POD_NAMESPACE")
	// if len(ns) > 0 {
	// 	addr += "." + ns + "svc.cluster.local"
	// }

	if opts.Address[:1] == ":" {
		addr += opts.Address
	}

	return func() (*registry.Node, error) {
		return &registry.Node{
			Address: addr,
		}, nil
	}, nil

	return nil, errors.InternalServerError("go.micro.client", "TODO service address")
}

func (h *httpClient) call(ctx context.Context, node *registry.Node, req client.Request, rsp interface{}, opts client.CallOptions) error {
	// set the address
	address := node.Address
	if node.Port > 0 {
		address = fmt.Sprintf("%s:%d", address, node.Port)
	}

	// get codec
	cf, err := h.newHTTPCodec(req.ContentType())
	if err != nil {
		return errors.InternalServerError("go.micro.client", err.Error())
	}

	// marshal request
	b, err := cf.Marshal(req.Body())
	if err != nil {
		return errors.InternalServerError("go.micro.client", err.Error())
	}

	buf := &buffer{bytes.NewBuffer(b)}
	defer buf.Close()

	url := &url.URL{
		Scheme: "http",
		Host:   address,
		Path:   req.Method(),
	}
	hreq, err := http.NewRequest("POST", url.String(), buf)
	if err != nil {
		return errors.InternalServerError("go.micro.client", err.Error())
	}

	// Header
	if md, ok := metadata.FromContext(ctx); ok {
		for k, v := range md {
			hreq.Header.Set(k, v)
		}
	}

	// set timeout in nanoseconds
	hreq.Header.Set("Timeout", fmt.Sprintf("%d", opts.RequestTimeout))
	// set the content type for the request
	hreq.Header.Set("Content-Type", req.ContentType())

	// TODO 自定义Client https://colobu.com/2018/03/08/two-issues-in-go-development/
	// make the request
	c := http.DefaultClient
	hrsp, err := c.Do(hreq.WithContext(ctx))
	if err != nil {
		return errors.InternalServerError("go.micro.client", err.Error())
	}
	defer hrsp.Body.Close()

	// parse response
	b, err = ioutil.ReadAll(hrsp.Body)
	if err != nil {
		return errors.InternalServerError("go.micro.client", err.Error())
	}

	// unmarshal
	if err := cf.Unmarshal(b, rsp); err != nil {
		return errors.InternalServerError("go.micro.client", string(b)+err.Error())
	}

	return nil
}

func (h *httpClient) stream(ctx context.Context, address string, req client.Request, opts client.CallOptions) (client.Stream, error) {
	header := make(http.Header)
	if md, ok := metadata.FromContext(ctx); ok {
		for k, v := range md {
			header.Set(k, v)
		}
	}

	// set timeout in nanoseconds
	header.Set("Timeout", fmt.Sprintf("%d", opts.RequestTimeout))
	// set the content type for the request
	header.Set("Content-Type", req.ContentType())

	// get codec
	cf, err := h.newHTTPCodec(req.ContentType())
	if err != nil {
		return nil, errors.InternalServerError("go.micro.client", err.Error())
	}

	cc, err := net.Dial("tcp", address)
	if err != nil {
		return nil, errors.InternalServerError("go.micro.client", fmt.Sprintf("Error dialing: %v", err))
	}

	return &httpStream{
		address: address,
		context: ctx,
		closed:  make(chan bool),
		conn:    cc,
		codec:   cf,
		header:  header,
		reader:  bufio.NewReader(cc),
		request: req,
	}, nil
}

func (h *httpClient) newHTTPCodec(contentType string) (Codec, error) {
	if c, ok := defaultHTTPCodecs[contentType]; ok {
		return c, nil
	}
	return nil, fmt.Errorf("Unsupported Content-Type: %s", contentType)
}

func (h *httpClient) newCodec(contentType string) (codec.NewCodec, error) {
	if c, ok := h.opts.Codecs[contentType]; ok {
		return c, nil
	}
	if cf, ok := defaultRPCCodecs[contentType]; ok {
		return cf, nil
	}
	return nil, fmt.Errorf("Unsupported Content-Type: %s", contentType)
}

func (h *httpClient) Init(opts ...client.Option) error {
	for _, o := range opts {
		o(&h.opts)
	}
	return nil
}

func (h *httpClient) Options() client.Options {
	return h.opts
}

func (h *httpClient) NewMessage(topic string, msg interface{}, opts ...client.MessageOption) client.Message {
	return newHTTPMessage(topic, msg, "application/proto", opts...)
}

func (h *httpClient) NewRequest(service, method string, req interface{}, reqOpts ...client.RequestOption) client.Request {
	return newHTTPRequest(service, method, req, h.opts.ContentType, reqOpts...)
}

func (h *httpClient) Call(ctx context.Context, req client.Request, rsp interface{}, opts ...client.CallOption) error {
	// make a copy of call opts
	callOpts := h.opts.CallOptions
	for _, opt := range opts {
		opt(&callOpts)
	}

	// get next nodes from the selector
	next, err := h.next(req, callOpts)
	if err != nil {
		return err
	}

	// check if we already have a deadline
	d, ok := ctx.Deadline()
	if !ok {
		// no deadline so we create a new one
		ctx, _ = context.WithTimeout(ctx, callOpts.RequestTimeout)
	} else {
		// got a deadline so no need to setup context
		// but we need to set the timeout we pass along
		opt := client.WithRequestTimeout(d.Sub(time.Now()))
		opt(&callOpts)
	}

	// should we noop right here?
	select {
	case <-ctx.Done():
		return errors.New("go.micro.client", fmt.Sprintf("%v", ctx.Err()), 408)
	default:
	}

	// make copy of call method
	hcall := h.call

	// wrap the call in reverse
	for i := len(callOpts.CallWrappers); i > 0; i-- {
		hcall = callOpts.CallWrappers[i-1](hcall)
	}

	// return errors.New("go.micro.client", "request timeout", 408)
	call := func() error {
		// select next node
		node, err := next()
		if err != nil && err == selector.ErrNotFound {
			return errors.NotFound("go.micro.client", err.Error())
		} else if err != nil {
			return errors.InternalServerError("go.micro.client", err.Error())
		}

		// make the call
		err = hcall(ctx, node, req, rsp, callOpts)
		return err
	}

	ch := make(chan error, callOpts.Retries)
	go func() {
		ch <- call()
	}()

	select {
	case <-ctx.Done():
		return errors.New("go.micro.client", fmt.Sprintf("%v", ctx.Err()), 408)
	case err := <-ch:
		return err
	}
}

// TODO stream支持
func (h *httpClient) Stream(ctx context.Context, req client.Request, opts ...client.CallOption) (client.Stream, error) {
	// make a copy of call opts
	callOpts := h.opts.CallOptions
	for _, opt := range opts {
		opt(&callOpts)
	}

	// get next nodes from the selector
	next, err := h.next(req, callOpts)
	if err != nil {
		return nil, err
	}

	// check if we already have a deadline
	d, ok := ctx.Deadline()
	if !ok {
		// no deadline so we create a new one
		ctx, _ = context.WithTimeout(ctx, callOpts.RequestTimeout)
	} else {
		// got a deadline so no need to setup context
		// but we need to set the timeout we pass along
		opt := client.WithRequestTimeout(d.Sub(time.Now()))
		opt(&callOpts)
	}

	// should we noop right here?
	select {
	case <-ctx.Done():
		return nil, errors.New("go.micro.client", fmt.Sprintf("%v", ctx.Err()), 408)
	default:
	}

	call := func(i int) (client.Stream, error) {
		// call backoff first. Someone may want an initial start delay
		t, err := callOpts.Backoff(ctx, req, i)
		if err != nil {
			return nil, errors.InternalServerError("go.micro.client", err.Error())
		}

		// only sleep if greater than 0
		if t.Seconds() > 0 {
			time.Sleep(t)
		}

		node, err := next()
		if err != nil && err == selector.ErrNotFound {
			return nil, errors.NotFound("go.micro.client", err.Error())
		} else if err != nil {
			return nil, errors.InternalServerError("go.micro.client", err.Error())
		}

		addr := node.Address
		if node.Port > 0 {
			addr = fmt.Sprintf("%s:%d", addr, node.Port)
		}

		stream, err := h.stream(ctx, addr, req, callOpts)
		h.opts.Selector.Mark(req.Service(), node, err)
		return stream, err
	}

	type response struct {
		stream client.Stream
		err    error
	}

	ch := make(chan response, callOpts.Retries)
	var grr error

	for i := 0; i < callOpts.Retries; i++ {
		go func() {
			s, err := call(i)
			ch <- response{s, err}
		}()

		select {
		case <-ctx.Done():
			return nil, errors.New("go.micro.client", fmt.Sprintf("%v", ctx.Err()), 408)
		case rsp := <-ch:
			// if the call succeeded lets bail early
			if rsp.err == nil {
				return rsp.stream, nil
			}

			retry, rerr := callOpts.Retry(ctx, req, i, err)
			if rerr != nil {
				return nil, rerr
			}

			if !retry {
				return nil, rsp.err
			}

			grr = rsp.err
		}
	}

	return nil, grr
}

func (h *httpClient) Publish(ctx context.Context, p client.Message, opts ...client.PublishOption) error {
	md, ok := metadata.FromContext(ctx)
	if !ok {
		md = make(map[string]string)
	}
	md["Content-Type"] = p.ContentType()

	cf, err := h.newCodec(p.ContentType())
	if err != nil {
		return errors.InternalServerError("go.micro.client", err.Error())
	}

	b := &buffer{bytes.NewBuffer(nil)}
	if err := cf(b).Write(&codec.Message{Type: codec.Publication}, p.Payload()); err != nil {
		return errors.InternalServerError("go.micro.client", err.Error())
	}

	h.once.Do(func() {
		h.opts.Broker.Connect()
	})

	return h.opts.Broker.Publish(p.Topic(), &broker.Message{
		Header: md,
		Body:   b.Bytes(),
	})
}

func (h *httpClient) String() string {
	return "http"
}

func newClient(opts ...client.Option) client.Client {
	options := client.Options{
		CallOptions: client.CallOptions{
			Backoff:        client.DefaultBackoff,
			Retry:          client.DefaultRetry,
			Retries:        client.DefaultRetries,
			RequestTimeout: client.DefaultRequestTimeout,
			DialTimeout:    transport.DefaultDialTimeout,
		},
	}

	for _, o := range opts {
		o(&options)
	}

	if len(options.ContentType) == 0 {
		options.ContentType = "application/proto"
	}

	if options.Broker == nil {
		options.Broker = broker.DefaultBroker
	}

	// if options.Registry == nil {
	// 	options.Registry = registry.DefaultRegistry
	// }

	if options.Selector == nil {
		options.Selector = selector.NewSelector(
			selector.Registry(options.Registry),
		)
	}

	rc := &httpClient{
		once: sync.Once{},
		opts: options,
	}

	c := client.Client(rc)

	// wrap in reverse
	for i := len(options.Wrappers); i > 0; i-- {
		c = options.Wrappers[i-1](c)
	}

	return c
}

func NewClient(opts ...client.Option) client.Client {
	return newClient(opts...)
}
