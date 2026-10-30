// Logs from eRPC's StreamLogs on the bds stubs, applied to a store keyed by
// (blockHash, logIndex) and resumed from erpc.cursor, with the toolkit rules
// done by hand: window, heartbeat watchdog, reconnect, undo, out of range.
// Exits after BLOCKS blocks (default: never).
//
//   ERPC_ENDPOINT=grpc://localhost:4001/main/evm/1 ERPC_ADDRESS=0x… mvn -q compile exec:java
package example;

import bds.evm.Models.BlockRef;
import bds.evm.Models.Log;
import bds.evm.Query.LogFilter;
import bds.evm.Stream.Finality;
import bds.evm.Stream.Reorg;
import bds.evm.Stream.StreamLogsRequest;
import bds.evm.Stream.StreamLogsResponse;
import bds.evm.StreamServiceGrpc;
import com.google.protobuf.Any;
import com.google.protobuf.ByteString;
import com.google.protobuf.InvalidProtocolBufferException;
import com.google.rpc.ErrorInfo;
import io.grpc.Context;
import io.grpc.ManagedChannel;
import io.grpc.Metadata;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import io.grpc.netty.shaded.io.grpc.netty.NettyChannelBuilder;
import io.grpc.protobuf.StatusProto;
import io.grpc.stub.MetadataUtils;
import java.io.IOException;
import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.NoSuchFileException;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.nio.file.StandardOpenOption;
import java.util.ArrayList;
import java.util.EnumSet;
import java.util.HashMap;
import java.util.HexFormat;
import java.util.Iterator;
import java.util.List;
import java.util.Map;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.ThreadLocalRandom;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicLong;

public final class StreamLogs {
  /** Seconds; the watchdog reconnects after 3 silent ones. */
  static final int HEARTBEAT = 15;
  static final Path CURSOR_PATH = Path.of("erpc.cursor");
  static final EnumSet<Status.Code> TERMINAL = EnumSet.of(Status.Code.OUT_OF_RANGE, Status.Code.ABORTED,
      Status.Code.UNAUTHENTICATED, Status.Code.PERMISSION_DENIED, Status.Code.INVALID_ARGUMENT);
  static final HexFormat HEX = HexFormat.of();

  /** A parsed grpc(s)://host:port/<project>/evm/<chainId>. */
  record Target(String host, int port, boolean tls, String project, String chain) {
    static Target parse(String raw) {
      URI u = URI.create(raw);
      boolean tls = switch (u.getScheme()) {
        case "grpc" -> false;
        case "grpcs" -> true;
        default -> throw new IllegalArgumentException(raw + ": scheme must be grpc or grpcs");
      };
      String[] parts = u.getPath().replaceAll("^/|/$", "").split("/");
      if (parts.length != 3 || !parts[1].equals("evm")) {
        throw new IllegalArgumentException(raw + ": path must be /<project>/evm/<chainId>");
      }
      int port = u.getPort() > 0 ? u.getPort() : tls ? 443 : 80;
      return new Target(u.getHost(), port, tls, parts[0], parts[2]);
    }
  }

  /** How a session ended: code is null when done, "restart", "watchdog" or a gRPC code name. */
  record End(boolean flowed, String code, StatusRuntimeException error) {}

  static String token(long number, ByteString hash) {
    return "0x" + Long.toHexString(number) + ":0x" + HEX.formatHex(hash.toByteArray());
  }

  /** The app: its logs and the cursor of the last block it applied. */
  static final class App {
    record Key(ByteString blockHash, int logIndex) {}

    record Held(long block, ByteString data) {}

    final Map<Key, Held> logs = new HashMap<>();
    final int limit;
    Long cursorNumber;
    ByteString cursorHash;
    int blocks;

    App(int limit) throws IOException {
      this.limit = limit;
      try {
        String[] t = Files.readString(CURSOR_PATH).trim().split(":");
        cursorNumber = Long.parseUnsignedLong(t[0].substring(2), 16);
        cursorHash = ByteString.copyFrom(HEX.parseHex(t[1].substring(2)));
      } catch (NoSuchFileException e) {
        // no cursor: the stream starts at the server's bound
      }
    }

    /** Writes a temporary file, syncs it and renames it over the cursor. */
    void save(long number, ByteString hash) throws IOException {
      Path tmp = Path.of(CURSOR_PATH + ".tmp");
      Files.writeString(tmp, token(number, hash) + "\n", StandardCharsets.UTF_8, StandardOpenOption.CREATE,
          StandardOpenOption.TRUNCATE_EXISTING, StandardOpenOption.WRITE, StandardOpenOption.SYNC);
      Files.move(tmp, CURSOR_PATH, StandardCopyOption.REPLACE_EXISTING, StandardCopyOption.ATOMIC_MOVE);
      cursorNumber = number;
      cursorHash = hash;
    }

    /** Idempotent: a block applied again (a crash before the cursor save) writes the same keys. */
    void apply(BlockRef block, List<Log> blockLogs) throws IOException {
      for (Log l : blockLogs) {
        logs.put(new Key(l.getBlockHash(), l.getLogIndex()), new Held(block.getNumber(), l.getData()));
      }
      save(block.getNumber(), block.getHash());
      blocks++;
      System.out.printf("block %d 0x%s: %d logs%n", block.getNumber(), HEX.formatHex(block.getHash().toByteArray()),
          blockLogs.size());
    }

    void undo(BlockRef forkPoint) throws IOException {
      logs.values().removeIf(h -> h.block() > forkPoint.getNumber());
      save(forkPoint.getNumber(), forkPoint.getHash());
      System.out.printf("undo to %d 0x%s%n", forkPoint.getNumber(), HEX.formatHex(forkPoint.getHash().toByteArray()));
    }

    boolean done() {
      return limit > 0 && blocks >= limit;
    }
  }

  /** One stream from the cursor until it ends. */
  static End session(App app, Target t, LogFilter filter, String secret) throws IOException {
    // The server's 4 MiB window (grpc-java sets one for streams and the
    // connection): with the 64 KiB default, log bursts wait on this client.
    NettyChannelBuilder b = NettyChannelBuilder.forAddress(t.host(), t.port()).flowControlWindow(4 << 20);
    // A fresh channel per session: a black-holed connection is never reused.
    ManagedChannel channel = (t.tls() ? b.useTransportSecurity() : b.usePlaintext()).build();
    Metadata md = new Metadata();
    md.put(Metadata.Key.of("x-erpc-project", Metadata.ASCII_STRING_MARSHALLER), t.project());
    md.put(Metadata.Key.of("x-erpc-chain-id", Metadata.ASCII_STRING_MARSHALLER), t.chain());
    if (secret != null) {
      md.put(Metadata.Key.of("x-erpc-secret-token", Metadata.ASCII_STRING_MARSHALLER), secret);
    }
    StreamLogsRequest.Builder req = StreamLogsRequest.newBuilder().setFilter(filter)
        .setFinality(Finality.FINALITY_LATEST).setReorg(Reorg.REORG_UNDO).setHeartbeatSeconds(HEARTBEAT);
    if (app.cursorNumber != null) {
      req.setFromBlock(app.cursorNumber).setFromHash(app.cursorHash);
    }
    Context.CancellableContext ctx = Context.current().withCancellation();
    ScheduledExecutorService timer = Executors.newSingleThreadScheduledExecutor();
    AtomicLong last = new AtomicLong(System.nanoTime());
    AtomicBoolean fired = new AtomicBoolean();
    long limit = TimeUnit.SECONDS.toNanos(3L * HEARTBEAT);
    timer.scheduleAtFixedRate(() -> {
      if (System.nanoTime() - last.get() > limit && fired.compareAndSet(false, true)) {
        ctx.cancel(null);
      }
    }, limit / 12, limit / 12, TimeUnit.NANOSECONDS);
    Context previous = ctx.attach();
    boolean flowed = false;
    BlockRef part = null; // the block in progress and its logs so far
    List<Log> partLogs = new ArrayList<>();
    try {
      Iterator<StreamLogsResponse> it = StreamServiceGrpc.newBlockingStub(channel)
          .withInterceptors(MetadataUtils.newAttachHeadersInterceptor(md)).streamLogs(req.build());
      while (it.hasNext()) {
        StreamLogsResponse msg = it.next();
        flowed = true;
        last.set(System.nanoTime());
        switch (msg.getMessageCase()) {
          case BLOCK -> {
            BlockRef ref = msg.getBlock().getBlock();
            if (part == null || !part.getHash().equals(ref.getHash())) {
              part = ref;
              partLogs = new ArrayList<>();
            }
            partLogs.addAll(msg.getBlock().getLogsList());
            if (!msg.getBlock().getComplete()) {
              continue;
            }
            part = null;
            if (ref.getHash().equals(app.cursorHash)) {
              continue; // already applied: skip
            }
            if (app.cursorHash != null && !ref.getParentHash().equals(app.cursorHash)) {
              return new End(flowed, "restart", null); // linkage broke: resume from the cursor
            }
            app.apply(ref, partLogs);
          }
          case UNDO -> {
            part = null;
            app.undo(msg.getUndo().getForkPoint());
          }
          case STATUS -> System.out.printf("status %s %s%n", msg.getStatus().getState(), msg.getStatus().getReason());
          default -> {}
        }
        last.set(System.nanoTime());
        if (app.done()) {
          return new End(flowed, null, null);
        }
      }
      return new End(flowed, "UNAVAILABLE", null); // the server closed the stream
    } catch (StatusRuntimeException e) {
      if (fired.get()) {
        System.out.println("watchdog: no message within 3 heartbeats");
        return new End(flowed, "watchdog", null);
      }
      return new End(flowed, e.getStatus().getCode().name(), e);
    } finally {
      ctx.detach(previous);
      ctx.cancel(null);
      timer.shutdownNow();
      channel.shutdownNow();
    }
  }

  /** The out-of-range end as a message: the cursor and the oldest block a stream can resume from. */
  static String outOfRange(StatusRuntimeException e, App app) {
    String from = app.cursorHash == null ? "none" : token(app.cursorNumber, app.cursorHash);
    com.google.rpc.Status st = StatusProto.fromThrowable(e);
    for (Any d : st == null ? List.<Any>of() : st.getDetailsList()) {
      if (!d.is(ErrorInfo.class)) {
        continue;
      }
      try {
        ErrorInfo info = d.unpack(ErrorInfo.class);
        long n = Long.parseUnsignedLong(info.getMetadataOrDefault("oldestBlock", "0"));
        return String.format("cursor %s out of range (%s); oldest resumable 0x%x:%s", from,
            info.getReason(), n, info.getMetadataOrDefault("oldestHash", ""));
      } catch (InvalidProtocolBufferException ignored) {
        // not an ErrorInfo after all; fall through to the status message
      }
    }
    return "cursor " + from + " out of range: " + e.getStatus().getDescription();
  }

  public static void main(String[] args) throws Exception {
    Target t = Target.parse(System.getenv("ERPC_ENDPOINT"));
    LogFilter.Builder filter = LogFilter.newBuilder();
    String address = System.getenv("ERPC_ADDRESS");
    if (address != null) {
      filter.addAddress(ByteString.copyFrom(HEX.parseHex(address.substring(2))));
    }
    String blocks = System.getenv("BLOCKS");
    App app = new App(blocks == null ? 0 : Integer.parseInt(blocks));
    int attempt = 0;
    while (true) {
      long began = System.nanoTime();
      End end = session(app, t, filter.build(), System.getenv("ERPC_TOKEN"));
      if (end.code() == null) {
        return;
      }
      if (end.error() != null && TERMINAL.contains(end.error().getStatus().getCode())) {
        if (end.error().getStatus().getCode() == Status.Code.OUT_OF_RANGE) {
          // Away longer than the server's window. Backfill the blocks after
          // the cursor through the oldest resumable block with eth_getLogs,
          // save that block's token to erpc.cursor, then start again.
          System.err.println(outOfRange(end.error(), app));
          System.exit(1);
        }
        throw end.error();
      }
      if (System.nanoTime() - began >= TimeUnit.SECONDS.toNanos(60)) {
        attempt = 0;
      }
      boolean atOnce = end.code().equals("restart")
          || end.flowed() && (end.code().equals("watchdog") || end.code().equals("UNAVAILABLE"));
      if (atOnce) {
        continue; // drained, aged out, stalled or silent: the cursor loses nothing
      }
      double delay = Math.min(30.0, 0.5 * Math.pow(2, Math.min(attempt, 6)));
      attempt++;
      System.out.printf("reconnect after %s%n", end.code());
      Thread.sleep((long) (1000 * (delay / 2 + ThreadLocalRandom.current().nextDouble(delay / 2))));
    }
  }
}
