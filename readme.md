“Go net/http Server Internals & DoS/Shutdown Behavior Study”

Analyzed net/http.Server internals: accept loop, goroutine-per-connection model, request lifecycle

Traced timeout propagation to net.Conn deadlines and verified with slowloris simulations

Designed reproducible experiments to measure goroutine, FD, latency under slow-client attacks

Implemented graceful shutdown scenarios and observed in-flight request handling with context timeouts

Used pprof/trace and OS-level tools to correlate Go runtime behavior with TCP connection states

Documented results with benchmarks and diagrams, linking to distributed system liveness and RPC node design