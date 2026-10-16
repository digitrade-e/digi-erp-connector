package hasavshevet

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

// A second writer on the same data directory — a second connector process, or
// a second service install pointed at the same ProgramData — must never issue
// a number: the import lock is in-process only, and two writers each reading
// the store gave 152 numbers to two keys in the gate's 2 × 200 run.
func TestReceiptWriter_SecondWriterOnOneDataDirIsRefused(t *testing.T) {
	h := newReceiptHarness(t, 1000)
	a := h.writer()

	b, err := NewReceiptWriter(h.cfg, noopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Recover(); !errors.Is(err, ErrReceiptDataInUse) {
		t.Fatalf("second Recover = %v; want ErrReceiptDataInUse", err)
	}
	if _, err := b.Write(context.Background(), receiptFor(testKey(1))); !errors.Is(err, ErrReceiptDataInUse) {
		t.Fatalf("second Write = %v; want ErrReceiptDataInUse", err)
	}
	if n := h.nextNumber(); n != 1000 || len(h.indexFiles()) != 0 || len(h.docs()) != 0 {
		t.Fatalf("a refused writer touched the install: next=%d index=%v docs=%v", n, h.indexFiles(), h.docs())
	}

	// The first one goes away (the other process exits): the second takes over
	// on its next request, without a restart.
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if res := mustWrite(t, b, receiptFor(testKey(1))); res.ReceiptNumber != 1000 {
		t.Fatalf("after the first closed, receipt = %d; want 1000", res.ReceiptNumber)
	}
	_ = b.Close()
}

// The same across processes: a connector holding the data directory makes this
// one refuse receipts, and when it exits this one takes over.
func TestReceiptWriter_AnotherProcessHoldingTheDataDirRefusesReceipts(t *testing.T) {
	if dir := os.Getenv("RECEIPT_LOCK_HELPER_DATADIR"); dir != "" {
		w, err := NewReceiptWriter(ReceiptWriterConfig{SendReceiptDir: os.Getenv("RECEIPT_LOCK_HELPER_SENDDIR"), DataDir: dir}, noopLogger{})
		if err == nil {
			err = w.Recover()
		}
		if err != nil {
			os.Stdout.WriteString("error " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("held\n")
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0) // no Close: the OS must drop the lock
	}

	h := newReceiptHarness(t, 1000)
	cmd := exec.Command(os.Args[0], "-test.run=^TestReceiptWriter_AnotherProcessHoldingTheDataDirRefusesReceipts$")
	cmd.Env = append(os.Environ(), "RECEIPT_LOCK_HELPER_DATADIR="+h.dataDir, "RECEIPT_LOCK_HELPER_SENDDIR="+h.sendDir)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "held\n" {
		_ = cmd.Process.Kill()
		t.Fatalf("helper said %q, %v", line, err)
	}

	w := h.newWriter()
	if _, err := w.Write(context.Background(), receiptFor(testKey(1))); !errors.Is(err, ErrReceiptDataInUse) {
		_ = cmd.Process.Kill()
		t.Fatalf("Write while another process holds the data dir = %v; want ErrReceiptDataInUse", err)
	}
	_ = stdin.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("helper did not exit")
	}
	if res := mustWrite(t, w, receiptFor(testKey(1))); res.ReceiptNumber != 1000 {
		t.Fatalf("receipt = %d; want 1000", res.ReceiptNumber)
	}
}
