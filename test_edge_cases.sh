#!/bin/bash

set -e

TEST_DIR="/tmp/bsync_test"
if [ -d "$TEST_DIR" ]
then
  rm -rf "$TEST_DIR"
fi
mkdir -p $TEST_DIR

# Test tracking variables
TOTAL_TESTS=0
PASSED_TESTS=0
FAILED_TESTS=0
FAILED_TEST_NAMES=()

# Function to create test files with different patterns
create_zero_file() {
  local file=$1
  local size=$2
  dd if=/dev/zero of="$file" bs=1M count=$((size / 1024 / 1024)) 2>/dev/null
}

create_random_file() {
  local file=$1
  local size=$2
  dd if=/dev/urandom of="$file" bs=1M count=$((size / 1024 / 1024)) 2>/dev/null
}

create_mixed_file() {
  local file=$1
  local size=$2
  # Create a file with some random data and some zeros
  dd if=/dev/urandom of="$file" bs=1M count=$((size / 1024 / 1024)) 2>/dev/null
  # Add some zero blocks at the beginning, middle, and end
  dd if=/dev/zero of="$file" bs=1M seek=0 count=1 conv=notrunc 2>/dev/null
  dd if=/dev/zero of="$file" bs=1M seek=$((size / 1024 / 1024 / 2)) count=1 conv=notrunc 2>/dev/null
  dd if=/dev/zero of="$file" bs=1M seek=$((size / 1024 / 1024 - 1)) count=1 conv=notrunc 2>/dev/null
}

create_bordered_zeros_file() {
  local file=$1
  local size=$2
  # Create file with zeros at borders and random data in middle
  dd if=/dev/zero of="$file" bs=1M count=$((size / 1024 / 1024)) 2>/dev/null
  # Add random data in the middle
  dd if=/dev/urandom of="$file" bs=1M seek=1 count=$((size / 1024 / 1024 - 2)) conv=notrunc 2>/dev/null
}

# Function to run a test
run_test() {
  local test_name="$1"
  local src_file="$2"
  local dst_file="$3"
  local expected_exit="$4"
  local expected_crc="$5"
  TOTAL_TESTS=$((TOTAL_TESTS + 1))
  echo "[TEST $TOTAL_TESTS] $test_name"
  echo "file src: $src_file"
  echo "file dst: $dst_file"

  # Run bsync and capture exit code
  set +e
  ./bsync -q -f "$src_file" -t "$dst_file"
  exit_code=$?
  set -e

  local test_passed=true

  if [ "$exit_code" = "$expected_exit" ]; then
    echo "✓ PASS: Exit code matches expected"
    # If expected to succeed, check md5sum of both files
    if [ "$expected_exit" = "0" ] && [ -f "$src_file" ] && [ -f "$dst_file" ]; then
      src_md5=$(md5sum "$src_file" | awk '{print $1}')
      dst_md5=$(md5sum "$dst_file" | awk '{print $1}')

      if [ "$src_md5" = "$dst_md5" ]; then
        echo "✓ PASS: Source and destination files match (md5sum: $src_md5)"
      else
        if [ "$expected_crc" = "1" ]
        then
          echo "✓ PASS: Source and destination files differ (src: $src_md5, dst: $dst_md5)"
          test_passed=true
        else
          echo "✗ FAIL: Source and destination files differ (src: $src_md5, dst: $dst_md5)"
          test_passed=false
        fi
      fi
    fi
  else
    echo "✗ FAIL: Expected exit code $expected_exit, got $exit_code"
    test_passed=false
  fi

  if [ "$test_passed" = true ]; then
    PASSED_TESTS=$((PASSED_TESTS + 1))
  else
    FAILED_TESTS=$((FAILED_TESTS + 1))
    FAILED_TEST_NAMES+=("$test_name")
  fi

  echo ""
}

# Test 1: Source file absent
run_test "Source file absent (should fail)" "/nonexistent/file" "$TEST_DIR/dst1.img" 1

# Test 2: Source file empty
touch "$TEST_DIR/empty_src.img"
run_test "Source file empty (should fail)" "$TEST_DIR/empty_src.img" "$TEST_DIR/dst2.img" 1

# Test 3: Source file with zeros
create_zero_file "$TEST_DIR/zero_src.img" 10485760 # 10MB
run_test "Source file all zeros, 10MB" "$TEST_DIR/zero_src.img" "$TEST_DIR/dst3.img" 0

# Test 4: Source file with random data
create_random_file "$TEST_DIR/random_src.img" 10485760 # 10MB
run_test "Source file all random, 10MB" "$TEST_DIR/random_src.img" "$TEST_DIR/dst4.img" 0

# Test 5: Source file with mixed data (zeros and random)
create_mixed_file "$TEST_DIR/mixed_src.img" 10485760 # 10MB
run_test "Source file mixed zeros and random, 10MB" "$TEST_DIR/mixed_src.img" "$TEST_DIR/dst5.img" 0

# Test 6: Source file with zeros at borders
create_bordered_zeros_file "$TEST_DIR/bordered_src.img" 10485760 # 10MB
run_test "Source file zeros at borders, random in middle, 10MB" "$TEST_DIR/bordered_src.img" "$TEST_DIR/dst6.img" 0

# Test 7: Destination directory doesn't exist
run_test "Destination directory does not exist (should fail)" "$TEST_DIR/random_src.img" "/nonexistent/dir/dst.img" 1

# Test 8: Destination file already exists and is empty
touch "$TEST_DIR/existing_empty_dst.img"
run_test "Destination file exists and is empty" "$TEST_DIR/random_src.img" "$TEST_DIR/existing_empty_dst.img" 0

# Test 9: Destination file already exists and is full of zeros
create_zero_file "$TEST_DIR/existing_zero_dst.img" 10485760 # 10MB
run_test "Destination file exists and is all zeros, 10MB" "$TEST_DIR/random_src.img" "$TEST_DIR/existing_zero_dst.img" 0

# Test 10: Destination file already exists with random data
create_random_file "$TEST_DIR/existing_random_dst.img" 10485760 # 10MB
run_test "Destination file exists and is all random, 10MB" "$TEST_DIR/random_src.img" "$TEST_DIR/existing_random_dst.img" 0

# Test 11: Destination file with bordered zeros
create_bordered_zeros_file "$TEST_DIR/existing_bordered_dst.img" 10485760 # 10MB
run_test "Destination file exists and is zeros at borders, 10MB" "$TEST_DIR/random_src.img" "$TEST_DIR/existing_bordered_dst.img" 0

# Test 12: Small source file (1MB)
create_random_file "$TEST_DIR/small_src.img" 1048576 # 1MB
run_test "Source file small random, 1MB" "$TEST_DIR/small_src.img" "$TEST_DIR/dst12.img" 0

# Test 13: Large source file (100MB)
create_random_file "$TEST_DIR/large_src.img" 104857600 # 100MB
run_test "Source file large random, 100MB" "$TEST_DIR/large_src.img" "$TEST_DIR/dst13.img" 0

# Test 14: Source file with zeros in middle
create_random_file "$TEST_DIR/middle_zeros_src.img" 10485760 # 10MB
# Add zero block in the middle
dd if=/dev/zero of="$TEST_DIR/middle_zeros_src.img" bs=1M seek=5 count=1 conv=notrunc 2>/dev/null
run_test "Source file random with zero block in middle, 10MB" "$TEST_DIR/middle_zeros_src.img" "$TEST_DIR/dst14.img" 0

# Test 15: Source file with zeros at start
create_random_file "$TEST_DIR/start_zeros_src.img" 10485760 # 10MB
# Add zero block at the start
dd if=/dev/zero of="$TEST_DIR/start_zeros_src.img" bs=1M seek=0 count=1 conv=notrunc 2>/dev/null
run_test "Source file random with zero block at start, 10MB" "$TEST_DIR/start_zeros_src.img" "$TEST_DIR/dst15.img" 0

# Test 16: Source file with zeros at end
create_random_file "$TEST_DIR/end_zeros_src.img" 10485760 # 10MB
# Add zero block at the end
dd if=/dev/zero of="$TEST_DIR/end_zeros_src.img" bs=1M seek=9 count=1 conv=notrunc 2>/dev/null
run_test "Source file random with zero block at end, 10MB" "$TEST_DIR/end_zeros_src.img" "$TEST_DIR/dst16.img" 0

# Test 17: Very small source file (1KB)
dd if=/dev/urandom of="$TEST_DIR/tiny_src.img" bs=1K count=1 2>/dev/null
run_test "Source file very small random, 1KB" "$TEST_DIR/tiny_src.img" "$TEST_DIR/dst17.img" 0

# Test 18: Source file with alternating zero and random blocks
create_random_file "$TEST_DIR/alternating_src.img" 10485760 # 10MB
# Add zero blocks at even positions
for i in 0 2 4 6 8; do
  dd if=/dev/zero of="$TEST_DIR/alternating_src.img" bs=1M seek=$i count=1 conv=notrunc 2>/dev/null
done
run_test "Source file alternating zero/random blocks, 10MB" "$TEST_DIR/alternating_src.img" "$TEST_DIR/dst18.img" 0

# Test 19: Destination file smaller than source
create_random_file "$TEST_DIR/smaller_dst.img" 5242880 # 5MB
run_test "Destination file smaller than source, 5MB" "$TEST_DIR/random_src.img" "$TEST_DIR/smaller_dst.img" 0

# Test 20: Destination file larger than source
create_random_file "$TEST_DIR/larger_dst.img" 20971520 # 20MB
run_test "Destination file larger than source, 20MB" "$TEST_DIR/random_src.img" "$TEST_DIR/larger_dst.img" 0

# Test 21: Source and destination are the same file
run_test "Source and destination are the same file" "$TEST_DIR/random_src.img" "$TEST_DIR/random_src.img" 0

# Test 22: Source file with sparse data (holes)
dd if=/dev/urandom of="$TEST_DIR/sparse_src.img" bs=1M count=1 2>/dev/null
# Create sparse file by seeking and writing
dd if=/dev/urandom of="$TEST_DIR/sparse_src.img" bs=1M seek=5 count=1 conv=notrunc 2>/dev/null
run_test "Source file sparse (holes), 6MB" "$TEST_DIR/sparse_src.img" "$TEST_DIR/dst22.img" 0

# Test 23: Source file with all zeros except one block
create_zero_file "$TEST_DIR/almost_zero_src.img" 10485760 # 10MB
# Add one random block in the middle
dd if=/dev/urandom of="$TEST_DIR/almost_zero_src.img" bs=1M seek=5 count=1 conv=notrunc 2>/dev/null
run_test "Source file all zeros except one random block, 10MB" "$TEST_DIR/almost_zero_src.img" "$TEST_DIR/dst23.img" 0

# Test 24: Source file with all random data except one zero block
create_random_file "$TEST_DIR/almost_random_src.img" 10485760 # 10MB
# Add one zero block in the middle
dd if=/dev/zero of="$TEST_DIR/almost_random_src.img" bs=1M seek=5 count=1 conv=notrunc 2>/dev/null
run_test "Source file all random except one zero block, 10MB" "$TEST_DIR/almost_random_src.img" "$TEST_DIR/dst24.img" 0

# Test 25: Source file with compressible data (repeating patterns)
# Create a file with repeating patterns
for i in {1..10}; do
  echo "This is a repeating pattern number $i that should compress well" >>"$TEST_DIR/compressible_src.img"
done
# Pad to 10MB
dd if=/dev/zero of="$TEST_DIR/compressible_src.img" bs=1M seek=0 count=10 conv=notrunc 2>/dev/null
run_test "Source file compressible (repeating pattern), 10MB" "$TEST_DIR/compressible_src.img" "$TEST_DIR/dst25.img" 0

# Test 26: Source file smaller than block size (5MB)
create_random_file "$TEST_DIR/smaller_than_block_src.img" 5242880 # 5MB
run_test "Source file smaller than block size, 5MB" "$TEST_DIR/smaller_than_block_src.img" "$TEST_DIR/dst26.img" 0

# Test 27: Source file with odd byte count (10MB + 1 byte)
create_random_file "$TEST_DIR/odd_bytes_src.img" 10485760 # 10MB
# Add one extra byte
echo "x" >>"$TEST_DIR/odd_bytes_src.img"
run_test "Source file odd byte count (10MB+1)" "$TEST_DIR/odd_bytes_src.img" "$TEST_DIR/dst27.img" 0

# Test 28: Source file with odd byte count smaller than block (5MB + 1 byte)
create_random_file "$TEST_DIR/odd_small_src.img" 5242880 # 5MB
# Add one extra byte
echo "x" >>"$TEST_DIR/odd_small_src.img"
run_test "Source file odd byte count small (5MB+1)" "$TEST_DIR/odd_small_src.img" "$TEST_DIR/dst28.img" 0

# Test 29: Source file exactly one block size (10MB)
create_random_file "$TEST_DIR/exact_block_src.img" 10485760 # 10MB
run_test "Source file exactly one block size, 10MB" "$TEST_DIR/exact_block_src.img" "$TEST_DIR/dst29.img" 0

# Test 30: Source file slightly larger than one block (10MB + 1KB)
create_random_file "$TEST_DIR/slightly_larger_src.img" 10485760 # 10MB
# Add 1KB extra
dd if=/dev/urandom of="$TEST_DIR/slightly_larger_src.img" bs=1K count=1 conv=notrunc 2>/dev/null
run_test "Source file slightly larger than one block (10MB+1KB)" "$TEST_DIR/slightly_larger_src.img" "$TEST_DIR/dst30.img" 0

# Test 31: Source file with non-aligned size (10MB + 1234 bytes)
create_random_file "$TEST_DIR/non_aligned_src.img" 10485760 # 10MB
# Add 1234 extra bytes
dd if=/dev/urandom of="$TEST_DIR/non_aligned_src.img" bs=1 count=1234 conv=notrunc 2>/dev/null
run_test "Source file non-aligned size (10MB+1234 bytes)" "$TEST_DIR/non_aligned_src.img" "$TEST_DIR/dst31.img" 0

# Test 32: Source file with very small size (1 byte)
echo "x" >"$TEST_DIR/tiny_byte_src.img"
run_test "Source file very small (1 byte)" "$TEST_DIR/tiny_byte_src.img" "$TEST_DIR/dst32.img" 0

# Test 33: Source file with size just under block size (10MB - 1 byte)
create_random_file "$TEST_DIR/just_under_block_src.img" 10485759 # 10MB - 1 byte
run_test "Source file just under block size (10MB-1)" "$TEST_DIR/just_under_block_src.img" "$TEST_DIR/dst33.img" 0

# Test 34: Source file with size just over block size (10MB + 1 byte)
create_random_file "$TEST_DIR/just_over_block_src.img" 10485760 # 10MB
# Add exactly one extra byte
dd if=/dev/urandom of="$TEST_DIR/just_over_block_src.img" bs=1 count=1 conv=notrunc 2>/dev/null
run_test "Source file just over block size (10MB+1)" "$TEST_DIR/just_over_block_src.img" "$TEST_DIR/dst34.img" 0

# Test 35: Source file with multiple non-aligned blocks (25MB + 777 bytes)
create_random_file "$TEST_DIR/multi_non_aligned_src.img" 26214400 # 25MB
# Add 777 extra bytes
dd if=/dev/urandom of="$TEST_DIR/multi_non_aligned_src.img" bs=1 count=777 conv=notrunc 2>/dev/null
run_test "Source file multiple non-aligned blocks (25MB+777 bytes)" "$TEST_DIR/multi_non_aligned_src.img" "$TEST_DIR/dst35.img" 0

# Test 36: Source file big random, 200MB
create_random_file "$TEST_DIR/big_src.img" 209715200 # 200MB
run_test "Source file big random, 200MB" "$TEST_DIR/big_src.img" "$TEST_DIR/dst_big.img" 0

# Test 37: Destination file larger than source, extra data is random
create_random_file "$TEST_DIR/large_random_dst.img" 20971520 # 20MB
run_test "Dest larger than source, extra data random" "$TEST_DIR/random_src.img" "$TEST_DIR/large_random_dst.img" 0

# Test 38: Destination file is sparse, source is full
create_random_file "$TEST_DIR/full_src.img" 10485760 # 10MB
cp "$TEST_DIR/full_src.img" "$TEST_DIR/sparse_dst.img"
truncate -s 20M "$TEST_DIR/sparse_dst.img"
run_test "Dest is sparse, source is full" "$TEST_DIR/full_src.img" "$TEST_DIR/sparse_dst.img" 0

# Test 39: Source file with all 0xFF bytes
ff_file="$TEST_DIR/ff_src.img"
dd if=/dev/zero bs=1M count=10 of="$ff_file" 2>/dev/null
perl -pi -e 's/\x00/\xFF/g' "$ff_file"
run_test "Source file all 0xFF bytes" "$ff_file" "$TEST_DIR/dst_ff.img" 0

# Test 40: Destination file is read-only
create_random_file "$TEST_DIR/readonly_dst.img" 10485760 # 10MB
chmod 444 "$TEST_DIR/readonly_dst.img"
run_test "Destination file is read-only (should fail)" "$TEST_DIR/random_src.img" "$TEST_DIR/readonly_dst.img" 1
chmod 644 "$TEST_DIR/readonly_dst.img"

# Test 41: Source file changes during sync (simulate by background write)
create_random_file "$TEST_DIR/changing_src.img" 10485760 # 10MB
(for i in 1 2 3 4 5; do sleep 1; dd if=/dev/urandom of="$TEST_DIR/changing_src.img" bs=1M count=1 seek=5 conv=notrunc 2>/dev/null; done ) &
run_test "Source file changes during sync" "$TEST_DIR/changing_src.img" "$TEST_DIR/dst_changing.img" 0 1
wait

# Test 42: Destination file is a device (simulate with /dev/null)
run_test "Destination is /dev/null (should fail or skip)" "$TEST_DIR/random_src.img" "/dev/null" 0

# Test 43: Source file with all byte values
allbytes="$TEST_DIR/allbytes_src.img"
perl -e 'print map { chr } 0..255' > "$allbytes"
run_test "Source file with all byte values" "$allbytes" "$TEST_DIR/dst_allbytes.img" 0

# Test 44: Destination file is a symlink
create_random_file "$TEST_DIR/random_src.img" 1048576
create_random_file "$TEST_DIR/real_dst.img" 10485760 # 10MB
ln -sf "$TEST_DIR/real_dst.img" "$TEST_DIR/symlink_dst.img"
run_test "Destination file is a symlink" "$TEST_DIR/random_src.img" "$TEST_DIR/symlink_dst.img" 0

# Test 45: Source file is a symlink
ln -sf "$TEST_DIR/random_src.img" "$TEST_DIR/symlink_src.img"
run_test "Source file is a symlink" "$TEST_DIR/symlink_src.img" "$TEST_DIR/dst_symlink.img" 0

# Edge: Destination file is locked by another process
# create_random_file "$TEST_DIR/locked_dst.img" 10485760 # 10MB
# (exec 9>"$TEST_DIR/locked_dst.img"; flock -x 9; run_test "Destination file locked by another process (should fail)" "$TEST_DIR/random_src.img" "$TEST_DIR/locked_dst.img" 1) &
# sleep 1
# wait

# Edge: Very large file (>4GB)
# Only run if you have enough disk space
# create_random_file "$TEST_DIR/huge_src.img" 4294967296 # 4GB
# run_test "Very large source file (>4GB)" "$TEST_DIR/huge_src.img" "$TEST_DIR/dst_huge.img" 0

echo "All edge case tests completed successfully!"
echo "Cleaning up test files..."
rm -rf "$TEST_DIR"

# Print test summary
echo ""
echo "=========================================="
echo "TEST SUMMARY"
echo "=========================================="
echo "Total tests run: $TOTAL_TESTS"
echo "Tests passed: $PASSED_TESTS"
echo "Tests failed: $FAILED_TESTS"
echo ""

if [ $FAILED_TESTS -gt 0 ]; then
  echo "Failed tests:"
  for test_name in "${FAILED_TEST_NAMES[@]}"; do
    echo "  - $test_name"
  done
  echo ""
  echo "Some tests failed. Please check the output above for details."
  exit 1
else
  echo "All tests passed! ✓"
fi

