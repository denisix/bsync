#!/bin/bash
src=/tmp/src.img
dst=/tmp/dst.img

rm -f $src $dst

dd if=/dev/random of=$src bs=1M count=100
bsync -f $src -t 127.0.0.1:$dst -b 1048576

ls -lsh $src $dst

if [ "$(md5sum $src | awk '{ print $1 }')" = "$(md5sum $dst | awk '{ print $1 }')" ]; then
  echo "OK, equal."
else
  echo "BAD, wrong."
fi
