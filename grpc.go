package sgrpc

import (
	"context"
	"encoding/base64"
	"fmt"
	"hash/crc32"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/ristretto"
	"github.com/golang/protobuf/proto"
	"github.com/subiz/errors"
	co "github.com/subiz/header/common"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	protoV2 "google.golang.org/protobuf/proto"
)

// CredKey is key which credential is putted in medatada.MD
const (
	CredKey  = "credential"
	CtxKey   = "pcontext"
	ErrKey   = "error"
	PanicKey = "panic"
)

// WithCache returns a DialOption which can cache the result of the grpc requests.
// The server control caching much like HTTP Cache-Control Header. The server attachs
// "max-age" header indicates how many seconds should the client cache the response.
// For that duration, the client would returns immediately the last response for those
// requests that has identical parameters.
//
// MAGIC: this interceptor may drop all call options, its trailing
// interceptors may also not be executed. We can remove this magic
// if we have better understanding of the go grpc library
func WithCache() grpc.DialOption {
	var cachedMethods sync.Map
	cache, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: 1e8,     // number of keys to track frequency of (100M).
		MaxCost:     1 << 30, // maximum cost of cache (1GB).
		BufferItems: 64,      // number of keys per Get buffer.
	})
	if err != nil {
		panic(err)
	}

	f := func(ctx context.Context, method string, req interface{}, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		// exit immediately if the method and its params match a cache item
		if _, ok := cachedMethods.Load(method); ok {
			key := proto.MarshalTextString(req.(proto.Message))
			if val, ok := cache.Get(key); ok {
				return proto.Unmarshal(val.([]byte), reply.(proto.Message))
			}
		}

		// cache miss, invoke request normally
		var header metadata.MD
		opts = append([]grpc.CallOption{grpc.Header(&header)}, opts...)
		err := invoker(ctx, method, req, reply, cc, opts...)
		if err != nil {
			return err
		}

		if len(header["max-age"]) > 0 {
			maxage, err := strconv.Atoi(header["max-age"][0])
			if err == nil {
				key := proto.MarshalTextString(req.(proto.Message))
				val, _ := proto.Marshal(reply.(proto.Message))
				if maxage > 0 {
					cachedMethods.Store(method, true)
					cache.SetWithTTL(key, val, 1, time.Duration(maxage)*time.Second)
				} else {
					cache.Del(key)
				}
			}
		}

		return nil
	}
	return grpc.WithUnaryInterceptor(f)
}

// WithShardRedirect creates a dial option that learns on "shard_addrs" reponse
// header to send requests to correct shard
// see https://www.notion.so/Shard-service-c002bcb0b00c47669bce547be646cd9f
// for the overall design
func WithShardRedirect() grpc.DialOption {
	lock := &sync.Mutex{}
	// GRPC connections to all shard workers, mapping host (user-3.user:2000) to connection
	conn := make(map[string]*grpc.ClientConn)
	// list of current shard worker addresses (order is important)
	addrs := []string{}

	f := func(ctx context.Context, method string, req interface{}, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		lock.Lock()

		// has data learned from last request
		if len(addrs) > 0 {
			// looking for shard key in header or account_id field in parameter
			md, _ := metadata.FromOutgoingContext(ctx)
			pkey := strings.Join(md["shard_key"], "")
			if pkey == "" {
				pkey = GetAccountId(ctx, req)
			}
			if pkey != "" {
				// finding the shard number
				shardNumber := int(crc32.ChecksumIEEE([]byte(pkey))) % len(addrs)
				host := addrs[shardNumber]
				co, ok := conn[host]
				if !ok {
					var err error

					co, err = grpc.Dial(host, grpc.WithInsecure())
					if err != nil {
						lock.Unlock()
						return err
					}
					conn[host] = co
				}
				lock.Unlock()
				var header metadata.MD // variable to store header and trailer
				opts = append([]grpc.CallOption{grpc.Header(&header)}, opts...)
				err := co.Invoke(ctx, method, req, reply, opts...)
				if len(header["shard_addrs"]) > 0 {
					lock.Lock()
					addrs = header.Get("shard_addrs")
					lock.Unlock()
				}
				return err
			}
		}

		lock.Unlock()
		// no sharding parameter, perform the request anyway
		var header metadata.MD
		opts = append([]grpc.CallOption{grpc.Header(&header)}, opts...)
		err := invoker(ctx, method, req, reply, cc, opts...)
		if len(header["shard_addrs"]) > 0 {
			lock.Lock()
			addrs = header.Get("shard_addrs")
			lock.Unlock()
		}
		return err
	}
	return grpc.WithUnaryInterceptor(f)
}

// SetMaxAge is used by the grpc server to tell clients the response isn't going
// to change for the next some seconds, and its safe to reuse this response
func SetMaxAge(ctx context.Context, sec int) {
	header := metadata.Pairs("max-age", fmt.Sprintf("%d", sec))
	grpc.SendHeader(ctx, header)
}

func ToGrpcCtx(pctx *co.Context) context.Context {
	data, err := proto.Marshal(pctx)
	if err != nil {
		panic(fmt.Sprintf("unable to marshal cred, %v", pctx))
	}
	cred64 := base64.StdEncoding.EncodeToString(data)
	return metadata.NewOutgoingContext(
		context.Background(),
		metadata.Pairs(CtxKey, cred64))
}

func FromGrpcCtx(ctx context.Context) *co.Context {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		md, ok = metadata.FromOutgoingContext(ctx)
		if !ok {
			return nil
		}
	}
	cred64 := strings.Join(md[CtxKey], "")
	if cred64 == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(cred64)
	if err != nil {
		panic(fmt.Sprintf("%v, %s: %s", err, "wrong base64 ", cred64))
	}

	pctx := &co.Context{}
	if err = proto.Unmarshal(data, pctx); err != nil {
		panic(fmt.Sprintf("%v, %s: %s", err, "unable to unmarshal cred ", cred64))
	}
	return pctx
}

func RecoverInterceptor(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (ret interface{}, err error) {
	func() {
		defer func() {
			if r := recover(); r != nil {
				e, ok := r.(error)
				if ok {
					err = errors.Wrap(e, 500, errors.E_unknown)
				}

				err = errors.New(500, errors.E_unknown, fmt.Sprintf("%v", e))
			}
		}()
		ret, err = handler(ctx, req)
	}()
	if err != nil {
		e, ok := err.(*errors.Error)
		if !ok {
			e, _ = errors.Wrap(err, 500, errors.E_unknown).(*errors.Error)
		}
		md := metadata.Pairs(PanicKey, e.Error())
		grpc.SendHeader(ctx, md)
	}
	return ret, err
}

// UnaryServerInterceptor returns a new unary server interceptor for panic recovery.
func NewRecoveryInterceptor() grpc.ServerOption {
	return grpc.UnaryInterceptor(RecoverInterceptor)
}

func GetPanic(md metadata.MD) *errors.Error {
	errs := strings.Join(md[PanicKey], "")
	if errs == "" {
		return nil
	}
	return errors.FromString(errs)
}

// forward proxy a GRPC calls to another host, header and trailer are preserved
// parameters:
//   host: host address which will be redirected to
//   method: the full RPC method string, i.e., /package.service/method.
//   returnedType: type of returned value
//   in: value of input (in request) parameter
// this method returns output just like a normal GRPC call
func forward(cc *grpc.ClientConn, method string, returnedType reflect.Type, ctx context.Context, in interface{}, extraHeader metadata.MD) (interface{}, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	extraHeader = metadata.Join(extraHeader, md)
	outctx := metadata.NewOutgoingContext(context.Background(), extraHeader)

	out := reflect.New(returnedType).Interface()
	var header, trailer metadata.MD
	err := cc.Invoke(outctx, method, in, out, grpc.Header(&header), grpc.Trailer(&trailer))
	grpc.SendHeader(ctx, header)
	grpc.SetTrailer(ctx, trailer)

	return out, err
}

// NewStatefulSetShardInterceptor create a shard interceptor compatible with kubernetes
// statefulset, each pod is a shard, shard number is extract from pod ordinal number
func NewStatefulSetShardInterceptor(grpcport, shards int) grpc.UnaryServerInterceptor {
	hostname, _ := os.Hostname()
	sp := strings.Split(hostname, "-")
	if len(sp) < 2 {
		panic("invalid hostname" + hostname)
	}
	ordinal := sp[len(sp)-1]
	pari64, _ := strconv.ParseInt(ordinal, 10, 0)
	ordinal_num := int(pari64)

	hosts := make([]string, 0)
	for i := 0; i < shards; i++ {
		// convo-${i}.convo:{port}
		hosts = append(hosts, sp[0]+"-"+strconv.Itoa(i)+"."+sp[0]+":"+strconv.Itoa(grpcport))
	}

	return NewServerShardInterceptor(hosts, ordinal_num)
}

// NewShardInterceptor makes a GRPC server intercepter that can be used for sharding
// see https://www.notion.so/Shard-service-c002bcb0b00c47669bce547be646cd9f
// for more details about the design
func NewServerShardInterceptor(serviceAddrs []string, id int) grpc.UnaryServerInterceptor {
	// holds the current maximum number of shards
	numShard := len(serviceAddrs)

	// in order to proxy (forward) the request to another grpc host,
	// we must have an output object of the request's method (so we can marshal the response).
	// we are going to build a map of returning type for all methods of the server. And do it
	// only once time for each method name right before the first request.
	returnedTypeM := make(map[string]reflect.Type)

	// GRPC connections to shard workers
	// mapping worker address (user-2.user:8080) to a GRPC connection
	lock := &sync.Mutex{}
	conn := make(map[string]*grpc.ClientConn)

	return func(ctx context.Context, in interface{}, sinfo *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (out interface{}, err error) {
		defer func() {
			if r := recover(); r != nil {
				e, ok := r.(error)
				if ok {
					err = errors.Wrap(e, 500, errors.E_unknown)
				}
				err = errors.New(500, errors.E_unknown, fmt.Sprintf("%v", e))
			}
		}()

		// looking for shard_key in header or in account_id parameter
		md, _ := metadata.FromIncomingContext(ctx)
		pkey := strings.Join(md["shard_key"], "")

		if pkey == "" {
			pkey = GetAccountId(ctx, in)
			if pkey == "" {
				// no sharding parameter, perform the request anyway
				return handler(ctx, in)
			}
		}

		// find the correct shard

		parindex := int(crc32.ChecksumIEEE([]byte(pkey))) % numShard

		// process if this is the correct shard
		if int(parindex) == id {
			return handler(ctx, in)
		}

		// the request have been proxied two times. We give up to prevent looping
		redirectOfRedirect := len(strings.Join(md["shard_redirected_2"], "")) > 0
		if redirectOfRedirect {
			return nil, status.Errorf(codes.Internal, "Sharding inconsistent")
		}

		// the request just have been proxied and still
		// doesn't arrived to the correct host
		// this happend when total_shards is not consistent between servers. We will wait for
		// 5 secs and then proxy one more time. Hoping that the consistent will be resolved
		justRedirect := len(strings.Join(md["shard_redirected"], "")) > 0
		extraHeader := metadata.New(nil)
		if justRedirect {
			// mark the the request have been proxied twice
			extraHeader.Set("shard_redirected_2", "true")
			time.Sleep(5 * time.Second)
		} else {
			// mark the the request have been proxied once
			extraHeader.Set("shard_redirected", "true")
		}

		header := metadata.New(nil)
		header.Set("shard_addrs", serviceAddrs...)
		grpc.SendHeader(ctx, header)

		// use cache host connection or create a new one
		host := serviceAddrs[parindex]
		lock.Lock()
		cc, ok := conn[host]

		if !ok {
			var err error
			cc, err = grpc.Dial(host, grpc.WithInsecure())
			if err != nil {
				lock.Unlock()
				return nil, err
			}
			conn[host] = cc
		}

		// making a map of returning type for all methods of the server
		returntype := returnedTypeM[sinfo.FullMethod]
		if returntype == nil {
			returntype = getReturnType(sinfo.Server, sinfo.FullMethod)
			returnedTypeM[sinfo.FullMethod] = returntype
		}
		lock.Unlock()
		return forward(cc, sinfo.FullMethod, returntype, ctx, in, extraHeader)
	}
}

// getReturnType returns the return types for a GRPC method
// the method name should be full method name (i.e., /package.service/method)
// For example, with handler
//   (s *server) func Goodbye() string {}
//   (s *server) func Ping(_ context.Context, _ *pb.Ping) (*pb.Pong, error) {}
//   (s *server) func Hello(_ context.Context, _ *pb.Empty) (*pb.String, error) {}
func getReturnType(server interface{}, fullmethod string) reflect.Type {
	t := reflect.TypeOf(server)
	for i := 0; i < t.NumMethod(); i++ {
		methodType := t.Method(i).Type

		if !strings.HasSuffix(fullmethod, "/"+t.Method(i).Name) {
			continue
		}

		if methodType.NumOut() != 2 || methodType.NumIn() < 2 {
			continue
		}

		// the first parameter should context and the second one should be a pointer
		if methodType.In(1).Name() != "Context" || methodType.In(2).Kind() != reflect.Ptr {
			continue
		}

		// the first output should be a pointer and the second one should be an error
		if methodType.Out(0).Kind() != reflect.Ptr || methodType.Out(1).Name() != "error" {
			continue
		}

		return methodType.Out(0).Elem()

	}
	return nil
}

func GetAccountId(ctx context.Context, message interface{}) string {
	msgrefl := message.(protoV2.Message).ProtoReflect()
	accIdDesc := msgrefl.Descriptor().Fields().ByName("account_id")
	accid := ""
	if accIdDesc == nil {
		accid = ""
	} else {
		accid = msgrefl.Get(accIdDesc).String()
	}

	if accid == "" {
		accid = FromGrpcCtx(ctx).GetCredential().GetAccountId()
	}

	return accid
}
