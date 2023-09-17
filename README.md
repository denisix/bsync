# bsync
Block sync between two server's devices

### Usage
To compile the program just issue:
```bash
make
```

Then copy `bsync` file to your servers.
Example usage to sync two block devices (or files) between servers:

1. on destination server where you need to sync to:
```bash
# ./bsync -f /dev/vda
- mode -> server file-> /dev/vda
- listening on 0.0.0.0:8080
```

2. on source server, from where you want to sync data:
```bash
# ./bsync -f /dev/sda -m client -r 1.2.3.4:8080
- mode -> client file-> /dev/sda
- startClient()
- getDeviceSize(): 4000787030016 bytes.
- source size: 4000787030016 bytes, block 104857600 bytes, blockNum: 38154
- block 1  /  38154
- connecting.. 178.23.189.124:49271
- block 2  /  38154
...
```
