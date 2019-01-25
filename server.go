package gubernator

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/mailgun/gubernator/lru"
	"github.com/mailgun/gubernator/pb"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
)

const (
	maxRequestSize = 1 * 1024 * 1024 // 1Mb
)

type Server struct {
	listener   net.Listener
	grpcServer *grpc.Server
	cache      *lru.Cache
	mutex      sync.Mutex
}

// New creates a gRPC server instance.
func NewServer(address string) (*Server, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, errors.Wrap(err, "failed to listen")
	}

	server := grpc.NewServer(grpc.MaxRecvMsgSize(maxRequestSize))
	s := Server{
		listener:   listener,
		grpcServer: server,
		// TODO: Set a limit on the size of the cache, so old entries expire
		cache: lru.NewLRUCache(0),
	}
	pb.RegisterRateLimitServiceServer(server, &s)
	return &s, nil
}

// Runs the gRPC server; blocks until server stops
func (s *Server) Run() error {
	// TODO: Perhaps allow resizing the cache on the fly depending on the number of cache hits
	// TODO: Emit metrics

	// TODO: Create PeerSync service that uses leader election and can sync the list of peers
	// TODO: Implement a GRPC interface to retrieve the peer listing from the CH for rate limit clients
	// TODO: Use https://github.com/nats-io/graft as the basis for leader election

	/*go func() {
		for {
			fmt.Printf("Size: %d\n", s.cache.Size())
			time.Sleep(time.Second)
		}
	}()*/

	return s.grpcServer.Serve(s.listener)
}

// Stops gRPC server
func (s *Server) Stop() {
	s.grpcServer.Stop()
}

// Return the address the server is listening too
func (s *Server) Address() string {
	return s.listener.Addr().String()
}

// Determine whether rate limiting should take place.
func (s *Server) ShouldRateLimit(ctx context.Context, req *pb.RateLimitRequest) (*pb.RateLimitResponse, error) {
	// TODO: Implement for generic clients

	// TODO: Optionally verify we are the owner of this key
	// TODO: Forward the request to the correct owner if needed
	return nil, nil
}

// Client implementations should use this method since they calculate the key and know which peer to use.
func (s *Server) ShouldRateLimitByKey(ctx context.Context, req *pb.RateLimitKeyRequest) (*pb.RateLimitResponse, error) {
	var results []*pb.DescriptorStatus
	for _, entry := range req.Entries {
		status, err := s.getRateLimt(ctx, entry)
		if err != nil {
			return nil, err
		}
		results = append(results, status)
	}
	return &pb.RateLimitResponse{Statuses: results}, nil
}

func (s *Server) getRateLimt(ctx context.Context, entry *pb.KeyRequestEntry) (*pb.DescriptorStatus, error) {
	// TODO: Optionally verify we are the owner of this key

	if entry.Hits == 0 {
		entry.Hits = 1
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()
	item, expire, ok := s.cache.Get(string(entry.Key))
	if ok {
		status := item.(*pb.DescriptorStatus)
		if status.Code == pb.DescriptorStatus_OVER_LIMIT {
			return status, nil
		}

		remaining := status.LimitRemaining - entry.Hits

		// If we are over our limit
		if remaining < 0 {
			status.OfHitsAccepted = status.CurrentLimit - status.LimitRemaining
			remaining = 0
			status.Code = pb.DescriptorStatus_OVER_LIMIT
		}
		status.LimitRemaining = remaining
		status.ResetTime = expire
		return status, nil
	}

	if entry.RateLimit == nil {
		return nil, errors.New("required field 'RateLimit' missing from 'KeyRequestEntry'")
	}

	now := time.Now().UTC().Unix()
	expire = now + entry.RateLimit.SpanInSeconds

	// Add a new rate limit
	status := &pb.DescriptorStatus{
		Code:           pb.DescriptorStatus_OK,
		CurrentLimit:   entry.RateLimit.RequestsPerSpan,
		LimitRemaining: entry.RateLimit.RequestsPerSpan - entry.Hits,
		ResetTime:      expire,
	}
	s.cache.Add(string(entry.Key), status, expire)

	return status, nil
}