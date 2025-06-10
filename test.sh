rm src.img dst.img
#killall bsync
dd if=/dev/zero of=src.img bs=10M count=10
dd if=/dev/random of=src.img bs=10M seek=5 count=5
dd if=/dev/zero of=dst.img bs=10M count=10

#./bsync -b 1048576 -f dst.img -p 34496 &
#sleep 1
time ./bsync -b 1048576 -f src.img -r 127.0.0.1:34496

sleep 4
ls -lsh src.img dst.img
md5sum src.img dst.img
#rm src.img dst.img
