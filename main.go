package main

import (
  "flag"
  "io"
  "fmt"
  "os"
  "net"
  "bufio"
  "time"
  "crypto/md5"
)

var debug bool = false
var blockSize uint32 = 104857600

func main() {
  var mode string
  var device string
  var servAddr string
  var bSize uint
  var skipIdx uint
  var port string

  flag.StringVar(&mode, "m", "server", "specify mode: 'server' or 'client'")
  flag.StringVar(&device, "f", "/dev/zero", "specify file or device, i.e. '/dev/vda'")
  flag.StringVar(&servAddr, "r", "127.0.0.1", "specify remote address of server")
  flag.UintVar(&bSize, "block", 104857600, "block size, default 100M")
  flag.UintVar(&skipIdx, "skip", 0, "skip blocks, default 0")
  flag.StringVar(&port, "p", "8080", "bind to port, default 8080")
  blockSize = uint32(bSize)

  flag.Parse()  // after declaring flags we need to call it

  fmt.Println("- mode ->", mode, "file->", device)

  if mode == "server" {

    file, err := os.OpenFile(device, os.O_RDWR|os.O_CREATE, 0666)
    if err != nil {
      fmt.Println("Error opening file:", err.Error())
      return
    }
    defer file.Close()

    startServer(file, port)

  } else if mode == "client" {

    file, err := os.OpenFile(device, os.O_RDONLY, 0666)
    if err != nil {
      fmt.Println("Error opening file:", err.Error())
      return
    }
    defer file.Close()

    startClient(file, servAddr, uint32(skipIdx))

  } else {

    fmt.Println("Invalid mode. Please specify 'server' or 'client'")
    os.Exit(1)

  }
}

func startServer(file *os.File, port string) {
  portStr := ":" + port
  listener, err := net.Listen("tcp", portStr)
  if err != nil {
    fmt.Println("Error listening:", err.Error())
    return
  }
  defer listener.Close()
  fmt.Println("- listening on 0.0.0.0" + portStr)

  for {
    conn, err := listener.Accept()
    if err != nil {
      fmt.Println("Error accepting: ", err.Error())
      return
    }
    go serverHandleReq(conn, file)
  }
}

// SERVER
func serverHandleReq(conn net.Conn, file *os.File) {
  fmt.Println("- serverHandleReq()")
  defer conn.Close()
  magicBytes := stringToFixedSizeArray(magicHead)

  filebuf := make([]byte, blockSize)
  netbuf1 := make([]byte, 17 + magicLen)

  var totBuf uint64 = 0
  var totRcvd uint64 = 0

  var percent float32 = 0
  var compressRatio float32 = 100
  var lastBlockNum uint32 = 0
  var lastPartSize uint64 = 0

  t0 := time.Now()
  mbs := 0.0
  var eta uint32 = 0

  c := bufio.NewReader(conn)

  for {
    _, err1 := io.ReadFull(c, netbuf1)
    if err1 != nil {
      fmt.Println("\t- connection closed:", err1)
      break
    }

    msg, err2 := unpack(netbuf1)
    if err2 != nil {
      fmt.Println("\t- unpack msg failed:", err2)
    }

    if msg.MagicHead != magicBytes {
      conn.Close()
      return
    }

    if debug { fmt.Println("\t- unpacked Msg->", msg) }

    offset := int64(msg.BlockIdx) * int64(blockSize)
    totBuf += uint64(blockSize)

    if msg.FileSize > 0 {
      percent = float32(uint64(offset) / (msg.FileSize / 100))

      compressRatio = float32(100 * float32(totRcvd) / float32(totBuf))

      if lastBlockNum == 0 {
        lastBlockNum = uint32(msg.FileSize / uint64(blockSize))
        lastPartSize = msg.FileSize - uint64(lastBlockNum) * uint64(blockSize)
        fmt.Println("- last part size ->", lastPartSize)
      }

      elapsedTime := time.Since(t0)
      elapsedSeconds := elapsedTime.Milliseconds()

      // fmt.Println("totBuf", totBuf, "elapsedSeconds", elapsedSeconds)
      mbs = (float64(totBuf) / 1048576.0) / (float64(elapsedSeconds) / 1000.0)

      eta = uint32(float64(msg.FileSize - totBuf) / 1048576.0 / mbs / 60)
    }

    if msg.DataSize > 0 {
      fmt.Printf("- block %d/%d (%0.2f%%) [w] size=%d ratio=%0.2f %0.2f MB/s ETA=%d min\n", msg.BlockIdx, lastBlockNum, percent, msg.FileSize, compressRatio, mbs, eta)
    } else {
      fmt.Printf("- block %d/%d (%0.2f%%) [-] size=%d ratio=%0.2f %0.2f MB/s ETA=%d min\n", msg.BlockIdx, lastBlockNum, percent, msg.FileSize, compressRatio, mbs, eta)
    }

    if msg.DataSize == 0 {
      if debug { fmt.Println("\t- read block from file") }

      n, err := file.ReadAt(filebuf, offset)
      if err != nil && err != io.EOF {
        fmt.Println("\t- error reading from file:", err.Error(), n)
        break
      }

      if debug { fmt.Println("\t- calc hash", n) }
      hash := md5.Sum(filebuf[:n])
      if debug { fmt.Println("\t- send hash", hash) }
      connWrite(conn, hash[:])
    }

    if msg.DataSize > 0 {
      if debug { fmt.Println("\t- read block from network", msg.DataSize) }

      _, err1 := io.ReadFull(c, filebuf[:msg.DataSize])
      if err1 != nil {
        fmt.Println("\t- connection closed:", err1)
        break
      }

      if msg.Compressed {
        decompressed, err := decompressData(filebuf[:msg.DataSize])
        if err != nil {
          fmt.Println("\t- error uncompressing:", err.Error())
          break
        }

        if debug { fmt.Println("\t- write uncompressed bytes:", len(decompressed), msg.DataSize) }
        n, err2 := file.WriteAt(decompressed, offset)
        if err2 != nil && err2 != io.EOF {
          fmt.Println("\t- error reading from file:", err2.Error(), n)
          break
        }
      } else {

        fmt.Println("\t- write non-compressed bytes:", msg.DataSize)
        n, err2 := file.WriteAt(filebuf[:msg.DataSize], offset)
        if err2 != nil && err2 != io.EOF {
          fmt.Println("\t- error reading from file:", err2.Error(), n)
          break
        }

      }

    }
    totRcvd = totRcvd + uint64(msg.DataSize)

    // last piece?
    if msg.BlockIdx > lastBlockNum {
      fmt.Println("- transferred compressed/real", totRcvd, "/", totBuf, "last part size ->", lastPartSize, "exiting..")

      os.Exit(0)
      // break
    }

  }
}
