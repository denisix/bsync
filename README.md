# bsync
Efficiently transfer large data between two block devices (or files) between servers, block by block: each block is compared by a checksum and transmitted with compression if they differ.

### Usage
To compile the program just issue:
```bash
make
```

Then copy `bsync` file to your servers.
Example usage to sync two block devices (or files) between servers:

1. on destination server where you need to sync to:
```bash
$ ./bsync -f /dev/shm/test-dst
- starting server, remote -> /dev/shm/test-dst (init blockSize = 104857600 )
- listening on 0.0.0.0:8080
- serverHandleReq()
- last part size -> 112524800 lastBlock 2 firstBlock 0 blockSize 204857600
- block 0/2 (0.00%) [-] size=522240000 ratio=0.00 0.00 MB/s ETA=0 min
- block 0/2 (0.00%) [w] size=522240000 ratio=30.71 0.00 MB/s ETA=0 min
- block 1/2 (39.00%) [-] size=522240000 ratio=0.00 67.99 MB/s ETA=0 min
- block 1/2 (39.00%) [w] size=522240000 ratio=21.55 40.86 MB/s ETA=0 min
- block 2/2 (78.00%) [-] size=522240000 ratio=0.00 65.11 MB/s ETA=0 min
- block 2/2 (78.00%) [w] size=522240000 ratio=14.05 57.40 MB/s ETA=0 min
- block 3/2 (117.00%) [-] size=522240000 ratio=0.00 80.50 MB/s ETA=0 min
- transfer done, exiting..
```

2. on source server, from where you want to sync data:
```bash
$ ./bsync -r 127.0.0.1:8080 -f /dev/shm/test-src -b 204857600
- starting client, transfer:  /dev/shm/test-src -> 127.0.0.1:8080
- startClient()
- getDeviceSize(): 522240000 bytes.
- source size: 522240000 bytes, block 204857600 bytes, blockNum: 2
- block 0 / 2
- connecting.. 127.0.0.1:8080
- block 1 / 2
- block 2 / 2
- block 3 / 2
- transfer done, exiting..
```

3. just to verify transferred correctly:
```bash
$ md5sum /dev/shm/test-src /dev/shm/test-dst
22aacaff71dc4477d917d449fdc2aa19  /dev/shm/test-src
22aacaff71dc4477d917d449fdc2aa19  /dev/shm/test-dst
```
