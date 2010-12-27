package mongogo


import (
    "gobson"
    "sync"
    "net"
    "os"
)


type replyFunc func(reply *replyOp, docNum int, docData []byte)

type mongoSocket struct {
    sync.Mutex
    server *mongoServer // nil when cached
    conn *net.TCPConn
    nextRequestId uint32
    replyFuncs map[uint32]replyFunc
    reserved bool
}

type queryOp struct {
    collection string
    query interface{}
    skip int32
    limit int32
    selector interface{}
    flags uint32
    replyFunc replyFunc
}

type replyOp struct {
    flags uint32
    cursorId int64
    firstDoc int32
    replyDocs int32
}

type insertOp struct {
    collection string       // "database.collection"
    documents []interface{} // One or more documents to insert
}

type requestInfo struct {
    bufferPos int
    replyFunc replyFunc
}

func newSocket(server *mongoServer, conn *net.TCPConn) *mongoSocket {
    socket := &mongoSocket{conn:conn}
    socket.replyFuncs = make(map[uint32]replyFunc)
    socket.Acquired(server)
    go socket.readLoop()
    return socket
}

// Inform the socket it's being put in use, either right after a
// connection or after being recycled.
func (socket *mongoSocket) Acquired(server *mongoServer) {
    socket.Lock()
    if socket.server != nil {
        panic("Attempting to reacquire an owned socket.")
    }
    socket.server = server
    socket.Unlock()
}

// Reserve the socket so that calling ImDone() will not put it back
// in its server's cache.
func (socket *mongoSocket) Reserve() {
    socket.Lock()
    socket.reserved = true
    socket.Unlock()
}

// Recycle the socket if it's not reserved.
func (socket *mongoSocket) ImDone() {
    socket.Lock()
    defer socket.Unlock()
    if !socket.reserved {
        socket.unlockedRecycle()
    }
}

// Recycle socket, putting it back into its server's cache.
func (socket *mongoSocket) Recycle() {
    socket.Lock()
    defer socket.Unlock()
    socket.unlockedRecycle()
}

func (socket *mongoSocket) unlockedRecycle() {
    server := socket.server
    socket.reserved = false
    socket.server = nil
    server.RecycleSocket(socket)
}

func (socket *mongoSocket) Query(ops ...interface{}) (err os.Error) {

    buf := make([]byte, 0, 256)

    // Serialize operations synchronously to avoid interrupting
    // other goroutines while we can't really be sending data.
    // Also, record id positions so that we can compute request
    // ids at once later with the lock already held.
    requests := make([]requestInfo, len(ops))
    requestCount := 0

    for _, op := range ops {
        start := len(buf)
        var replyFunc replyFunc
        switch op := op.(type) {
        case *insertOp:
            buf = addHeader(buf, 2002)
            buf = addInt32(buf, 0) // Reserved
            buf = addCString(buf, op.collection)
            for _, doc := range op.documents {
                buf, err = addBSON(buf, doc)
                if err != nil {
                    return err
                }
            }
        case *queryOp:
            buf = addHeader(buf, 2004)
            buf = addInt32(buf, int32(op.flags))
            buf = addCString(buf, op.collection)
            buf = addInt32(buf, op.skip)
            buf = addInt32(buf, op.limit)
            buf, err = addBSON(buf, op.query)
            if err != nil {
                return err
            }
            if op.selector != nil {
                buf, err = addBSON(buf, op.selector)
                if err != nil {
                    return err
                }
            }
            replyFunc = op.replyFunc
        }

        setInt32(buf, start, int32(len(buf) - start))

        if replyFunc != nil {
            request := &requests[requestCount]
            request.replyFunc = replyFunc
            request.bufferPos = start
            requestCount++
        }
    }

    // Buffer is ready for the pipe.  Lock, allocate ids, and enqueue.

    socket.Lock()

    // Reserve id 0 for requests which should have no responses.
    requestId := socket.nextRequestId + 1
    if requestId == 0 {
        requestId++
    }
    socket.nextRequestId = requestId + uint32(requestCount)
    for i := 0; i != requestCount; i++ {
        request := &requests[i]
        setInt32(buf, request.bufferPos + 4, int32(requestId))
        socket.replyFuncs[requestId] = request.replyFunc
        requestId++
    }

    // XXX Must check if server is set before doing this.
    debug("Sending ", len(ops), " op(s) (", len(buf), " bytes) to ",
          socket.server.Addr)

    _, err = socket.conn.Write(buf)
    socket.Unlock()
    return err
}

// Estimated minimum cost per socket: 1 goroutine + memory for the largest
// document ever seen.
func (socket *mongoSocket) readLoop() {
    // XXX How to handle locking on this method!?

    var prefixArray [36]byte // 16 from header + 20 from OP_REPLY fixed fields
    p := prefixArray[:]
    b := make([]byte, 256)
    conn := socket.conn
    for {
        // XXX Handle timeouts, EOFs, stopping, etc
        _, err := conn.Read(p)
        if err != nil {
            panic("Read error: " + err.String()) // XXX Do something here.
        }

        totalLen := getInt32(p, 0)
        responseTo := getInt32(p, 8)
        opCode := getInt32(p, 12)

        // XXX Must check if server is set before doing this.
        debug("Got reply (", totalLen, " bytes) from ", socket.server.Addr)

        _ = totalLen

        if opCode != 1 {
            // XXX Close the socket, rather than panicking.
            panic("Got a reply opcode != 1 from server. Corrupted data?")
        }

        reply := replyOp{flags:     uint32(getInt32(p, 16)),
                         cursorId:  getInt64(p, 20),
                         firstDoc:  getInt32(p, 28),
                         replyDocs: getInt32(p, 32)}

        socket.Lock()
        replyFunc, found := socket.replyFuncs[uint32(responseTo)]
        if found {
            socket.replyFuncs[uint32(responseTo)] = replyFunc, false
        }
        socket.Unlock()

        for i := 0; i != int(reply.replyDocs); i++ {
            conn.Read(b[:5])
            l := int(getInt32(b, 0))
            if cap(b) < l {
                newb := make([]byte, l)
                copy(newb, b[:5])
                b = newb
            } else {
                b = b[:l]
            }

            _, err = conn.Read(b[5:])
            if err != nil {
                panic(err.String()) // XXX Do something here.
            }

            if replyFunc != nil {
                replyFunc(&reply, i, b)
            }

            // XXX Do bound checking against totalLen.
        }

        // XXX Do bound checking against totalLen.
    }
}

var emptyHeader = []byte{0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0}

func addHeader(b []byte, opcode int) []byte {
    i := len(b)
    b = append(b, emptyHeader...)
    // This has to be changed for larger opcodes.
    b[i+12] = byte(opcode)
    b[i+13] = byte(opcode >> 8)
    return b
}

func addInt32(b []byte, i int32) []byte {
    return append(b, byte(i), byte(i>>8), byte(i>>16), byte(i>>24))
}

func addCString(b []byte, s string) []byte {
    b = append(b, []byte(s)...)
    b = append(b, 0)
    return b
}

func addBSON(b []byte, doc interface{}) ([]byte, os.Error) {
    data, err := gobson.Marshal(doc)
    if err != nil {
        return b, err
    }
    return append(b, data...), nil
}

func setInt32(b []byte, pos int, i int32) {
    b[pos]   = byte(i)
    b[pos+1] = byte(i>>8)
    b[pos+2] = byte(i>>16)
    b[pos+3] = byte(i>>24)
}

func getInt32(b []byte, pos int) int32 {
    return (int32(b[pos+0])) |
           (int32(b[pos+1]) << 8) |
           (int32(b[pos+2]) << 16) |
           (int32(b[pos+3]) << 24)
}

func getInt64(b []byte, pos int) int64 {
    return (int64(b[pos+0])) |
           (int64(b[pos+1]) << 8) |
           (int64(b[pos+2]) << 16) |
           (int64(b[pos+3]) << 24) |
           (int64(b[pos+4]) << 32) |
           (int64(b[pos+5]) << 40) |
           (int64(b[pos+6]) << 48) |
           (int64(b[pos+7]) << 56)
}
