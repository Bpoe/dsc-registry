namespace Dscregd.Tests;

using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Net;
using System.Net.Security;
using System.Net.Sockets;
using System.Security.Authentication;
using System.Security.Cryptography;
using System.Security.Cryptography.X509Certificates;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Channels;
using System.Threading.Tasks;
using Amqp;
using Amqp.Framing;
using Dscregd.Eventing;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;
using Microsoft.VisualStudio.TestTools.UnitTesting;

[TestClass]
public class EventingTests
{
    [TestMethod]
    public void DisabledHasNoWorkerChannelPublisherOrAllocation()
    {
        var services = new ServiceCollection();
        services.AddRegistryEventing(new ConfigurationBuilder().AddInMemoryCollection(new Dictionary<string, string?>
        {
            ["Eventing:Endpoint"] = "not-a-valid-endpoint",
            ["Eventing:Auth:PasswordEnv"] = "missing-password"
        }).Build());
        using var provider = services.BuildServiceProvider();
        Assert.AreEqual(0, provider.GetServices<IHostedService>().Count());
        Assert.IsNull(provider.GetService<EventWorker>());
        Assert.IsNull(provider.GetService<IEventPublisher>());
        var dispatcher = provider.GetRequiredService<EventDispatcher>();
        dispatcher.Notify("machine.created", "machine");
        var allocatedBefore = GC.GetAllocatedBytesForCurrentThread();
        for (var index = 0; index < 100; index++)
        {
            dispatcher.Notify("machine.created", "machine");
        }

        var allocatedAfter = GC.GetAllocatedBytesForCurrentThread();
        Assert.AreEqual(allocatedBefore, allocatedAfter);
    }

    [TestMethod]
    [DataRow(null, null, "machines/machine")]
    [DataRow("inventory", null, "machines/machine/configurations/inventory")]
    [DataRow("inventory", "report", "machines/machine/reports/report")]
    public void StructuredCloudEventAndAmqpUuidArePreserved(string? configuration, string? report, string subject)
    {
        var notification = new RegistryEvent("report.updated", "machine", configuration, report);
        using var message = AmqpEventPublisher.CreateMessage(notification);
        var encoded = message.Encode();
        using var decoded = Message.Decode(encoded);
        var messageId = decoded.Properties.GetMessageId();
        Assert.IsInstanceOfType<Guid>(messageId);
        Assert.AreEqual(notification.Id, (Guid)messageId);
        Assert.AreEqual(AmqpEventPublisher.ContentType, decoded.Properties.ContentType.ToString());
        Assert.AreEqual(typeof(Data), decoded.BodySection.GetType());
        var bytes = ((Data)decoded.BodySection).Binary;
        using var json = JsonDocument.Parse(bytes);
        var root = json.RootElement;
        Assert.AreEqual(8, root.EnumerateObject().Count());
        Assert.AreEqual("1.0", root.GetProperty("specversion").GetString());
        Assert.AreEqual(notification.Id, root.GetProperty("id").GetGuid());
        Assert.AreNotEqual(notification.Id, new RegistryEvent("report.updated", "machine").Id);
        Assert.AreEqual("dsc-registry", root.GetProperty("source").GetString());
        Assert.AreEqual("report.updated", root.GetProperty("type").GetString());
        Assert.AreEqual(subject, root.GetProperty("subject").GetString());
        Assert.AreEqual(TimeSpan.Zero, root.GetProperty("time").GetDateTimeOffset().Offset);
        Assert.AreEqual("application/json", root.GetProperty("datacontenttype").GetString());
        var data = root.GetProperty("data");
        Assert.AreEqual("machine", data.GetProperty("machineId").GetString());
        Assert.AreEqual(1 + (configuration == null ? 0 : 1) + (report == null ? 0 : 1), data.EnumerateObject().Count());
        if (configuration != null)
        {
            Assert.AreEqual(configuration, data.GetProperty("configurationName").GetString());
        }

        if (report != null)
        {
            Assert.AreEqual(report, data.GetProperty("reportId").GetString());
        }
    }

    [TestMethod]
    public void OnlyAcceptedDeliveryIsSuccessfulAndBackoffIsBounded()
    {
        Assert.IsTrue(AmqpEventPublisher.IsAccepted(new Accepted()));
        foreach (var outcome in new Outcome?[] { null, new Rejected(), new Released(), new Modified() })
        {
            Assert.IsFalse(AmqpEventPublisher.IsAccepted(outcome));
        }

        var backoff = TimeSpan.Zero;
        for (var index = 0; index < 20; index++)
        {
            backoff = AmqpEventPublisher.NextBackoff(backoff);
            Assert.IsTrue(backoff >= TimeSpan.FromMilliseconds(500) && backoff <= TimeSpan.FromSeconds(30));
            if (index == 0)
            {
                Assert.AreEqual(TimeSpan.FromMilliseconds(500), backoff);
            }
        }

        Assert.AreEqual(TimeSpan.FromSeconds(30), backoff);
    }

    [TestMethod]
    public async Task FullQueueDropsNewestWithoutBlockingAndDrainsOlderEvent()
    {
        var release = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        var publisher = new FakePublisher(async (notification, token) =>
        {
            await release.Task.WaitAsync(token);
        });
        var logger = new RecordingLogger<EventWorker>();
        using var worker = CreateWorker(publisher, logger, capacity: 1);
        var dispatcher = new EventDispatcher(worker);
        await worker.StartAsync(CancellationToken.None);
        dispatcher.Notify("machine.created", "first");
        Assert.AreEqual("first", (await publisher.ReadAsync()).Data.MachineId);
        dispatcher.Notify("machine.updated", "second");
        dispatcher.Notify("machine.deleted", "dropped");
        release.SetResult();
        await worker.StopAsync(CancellationToken.None);
        CollectionAssert.AreEqual(new[] { "first", "second" }, publisher.Notifications.Select(value => value.Data.MachineId).ToArray());
        Assert.AreEqual(2, logger.CountOutcome("queued"));
        Assert.AreEqual(1, logger.CountOutcome("queue_full"));
        Assert.AreEqual(2, logger.CountOutcome("published"));
        Assert.AreEqual(1, publisher.CloseCount);
        dispatcher.Notify("machine.created", "after-stop");
        await worker.StopAsync(CancellationToken.None);
        Assert.AreEqual(2, publisher.Notifications.Count);
        Assert.AreEqual(1, publisher.CloseCount);
    }

    [TestMethod]
    public async Task FailureIsRedactedDroppedOnceAndDoesNotKillWorker()
    {
        var publisher = new FakePublisher((notification, token) =>
        {
            return notification.Data.MachineId == "failed"
                ? Task.FromException(new InvalidOperationException("private-broker-password-and-payload"))
                : Task.CompletedTask;
        });
        var logger = new RecordingLogger<EventWorker>();
        using var worker = CreateWorker(publisher, logger);
        var dispatcher = new EventDispatcher(worker);
        await worker.StartAsync(CancellationToken.None);
        dispatcher.Notify("machine.created", "failed");
        dispatcher.Notify("machine.created", "accepted");
        await worker.StopAsync(CancellationToken.None);
        Assert.AreEqual(2, publisher.Notifications.Count);
        Assert.AreEqual(1, logger.CountOutcome("publish_failed"));
        Assert.AreEqual(1, logger.CountOutcome("published"));
        Assert.IsFalse(logger.Messages.Any(message => message.Contains("private-broker", StringComparison.Ordinal)));
        Assert.AreEqual(1, publisher.CloseCount);
    }

    [TestMethod]
    public async Task PublishDeadlineCancelsAttemptThenProcessesSubsequentNotification()
    {
        var publisher = new FakePublisher(async (notification, token) =>
        {
            if (notification.Data.MachineId == "blocked")
            {
                await Task.Delay(Timeout.InfiniteTimeSpan, token);
            }
        });
        var logger = new RecordingLogger<EventWorker>();
        using var worker = CreateWorker(publisher, logger, publishTimeout: TimeSpan.FromMilliseconds(50));
        var dispatcher = new EventDispatcher(worker);
        await worker.StartAsync(CancellationToken.None);
        dispatcher.Notify("machine.created", "blocked");
        await publisher.ReadAsync();
        dispatcher.Notify("machine.created", "subsequent");
        await worker.StopAsync(CancellationToken.None);
        Assert.AreEqual(2, publisher.Notifications.Count);
        Assert.AreEqual(1, logger.CountOutcome("publish_failed"));
        Assert.AreEqual(1, logger.CountOutcome("published"));
        Assert.IsTrue(publisher.AbortCount > 0);
    }

    [TestMethod]
    public async Task ShutdownHasAdditionalDeadlineCancelsActiveAndDiscardsPendingWithoutThrowing()
    {
        var publisher = new FakePublisher(async (notification, token) =>
        {
            await Task.Delay(Timeout.InfiniteTimeSpan, token);
        });
        var logger = new RecordingLogger<EventWorker>();
        using var worker = CreateWorker(publisher, logger, shutdownTimeout: TimeSpan.FromMilliseconds(50));
        var dispatcher = new EventDispatcher(worker);
        await worker.StartAsync(CancellationToken.None);
        dispatcher.Notify("machine.created", "active");
        await publisher.ReadAsync();
        dispatcher.Notify("machine.updated", "pending");
        await worker.StopAsync(CancellationToken.None).WaitAsync(TimeSpan.FromSeconds(3));
        Assert.AreEqual(1, publisher.Notifications.Count);
        Assert.AreEqual(1, publisher.CloseCount);
        Assert.IsTrue(logger.Messages.Any(message => message.Contains("timeout; pending discarded: 1", StringComparison.Ordinal)));
        Assert.AreEqual(1, logger.CountOutcome("publish_failed"));
    }

    [TestMethod]
    public async Task AlreadyCanceledHostDeadlineStillCleansUpWithoutShutdownFailure()
    {
        var publisher = new FakePublisher(async (notification, token) =>
        {
            await Task.Delay(Timeout.InfiniteTimeSpan, token);
        });
        using var worker = CreateWorker(publisher, new RecordingLogger<EventWorker>());
        await worker.StartAsync(CancellationToken.None);
        new EventDispatcher(worker).Notify("machine.created", "active");
        await publisher.ReadAsync();
        using var canceled = new CancellationTokenSource();
        canceled.Cancel();
        await worker.StopAsync(canceled.Token).WaitAsync(TimeSpan.FromSeconds(3));
        Assert.AreEqual(1, publisher.CloseCount);
    }

    [TestMethod]
    public async Task ConcurrentAdmissionAndStopHaveNoCompletionRace()
    {
        var publisher = new FakePublisher();
        using var worker = CreateWorker(publisher, new RecordingLogger<EventWorker>(), capacity: 3);
        var dispatcher = new EventDispatcher(worker);
        await worker.StartAsync(CancellationToken.None);
        var writers = Enumerable.Range(0, 12).Select(index => Task.Run(() =>
        {
            for (var notification = 0; notification < 100; notification++)
            {
                dispatcher.Notify("configuration.updated", "machine", "inventory");
            }
        })).ToArray();
        var stops = Enumerable.Range(0, 3).Select(index => worker.StopAsync(CancellationToken.None)).ToArray();
        await Task.WhenAll(writers.Concat(stops));
        Assert.AreEqual(1, publisher.CloseCount);
    }

    [TestMethod]
    public async Task ApplicationStoppingAndHttpDrainCanStillEnqueueBeforeWorkerStop()
    {
        var publisher = new FakePublisher();
        using var host = new HostBuilder().ConfigureServices(services =>
        {
            services.AddSingleton<IEventPublisher>(publisher);
            services.AddRegistryEventing(EnabledConfiguration());
            services.AddSingleton<IHostedService, SimulatedHttpServer>();
            services.Configure<HostOptions>(options =>
            {
                options.ShutdownTimeout = TimeSpan.FromSeconds(10);
                options.ServicesStopConcurrently = false;
            });
        }).Build();
        await host.StartAsync();
        var dispatcher = host.Services.GetRequiredService<EventDispatcher>();
        var lifetime = host.Services.GetRequiredService<IHostApplicationLifetime>();
        using var registration = lifetime.ApplicationStopping.Register(() =>
        {
            dispatcher.Notify("machine.created", "application-stopping");
        });
        await host.StopAsync();
        CollectionAssert.AreEqual(new[] { "application-stopping", "http-drain-commit" }, publisher.Notifications.Select(value => value.Data.MachineId).ToArray());
    }

    [TestMethod]
    public void EnabledConstructionValidatesLocalSecretBeforeListenerAndDoesNotDial()
    {
        var variable = "DSCREGISTRY_EVENTING_TEST_" + Guid.NewGuid().ToString("N");
        var services = new ServiceCollection();
        services.AddLogging();
        services.AddRegistryEventing(EnabledConfiguration(new Dictionary<string, string?>
        {
            ["Eventing:Auth:Type"] = "plain",
            ["Eventing:Auth:Username"] = "registry",
            ["Eventing:Auth:PasswordEnv"] = variable
        }));
        using var provider = services.BuildServiceProvider();
        var missing = Assert.ThrowsExactly<InvalidOperationException>(() => provider.GetServices<IHostedService>().ToArray());
        Assert.IsFalse(missing.Message.Contains(variable, StringComparison.Ordinal));
        var options = ValidOptions();
        options.Endpoint = "amqps://127.0.0.1:1";
        using var publisher = new AmqpEventPublisher(Options.Create(options), new RecordingLogger<AmqpEventPublisher>());
        Assert.IsNotNull(publisher);
    }

    [TestMethod]
    [DataRow("http://private-secret")]
    [DataRow("amqp://user:private-secret@host")]
    [DataRow("amqp://host/private-secret")]
    [DataRow("amqp://host?private-secret")]
    [DataRow("amqp://host#private-secret")]
    [DataRow("amqp://host:")]
    [DataRow("amqp://host:/")]
    [DataRow("amqp://[::1]:")]
    [DataRow("amqp://host: ")]
    [DataRow("amqp://host:0")]
    [DataRow("amqp://host:65536")]
    public void InvalidEndpointsHaveSafeConfigurationErrors(string endpoint)
    {
        var options = ValidOptions();
        options.Endpoint = endpoint;
        var error = Assert.ThrowsExactly<InvalidOperationException>(options.Validate);
        Assert.IsFalse(error.Message.Contains("private-secret", StringComparison.Ordinal));
    }

    [TestMethod]
    public void InvalidLocalCertificateMaterialFailsSafely()
    {
        var options = ValidOptions();
        options.Endpoint = "amqps://localhost";
        options.Tls.CaFile = "private-secret-unreadable-ca";
        var error = Assert.ThrowsExactly<InvalidOperationException>(() =>
        {
            using var publisher = new AmqpEventPublisher(Options.Create(options), new RecordingLogger<AmqpEventPublisher>());
        });
        Assert.IsFalse(error.Message.Contains("private-secret", StringComparison.Ordinal));
        using var certificates = new CertificateFiles();
        options.Tls.CaFile = certificates.InvalidPemPath;
        Assert.ThrowsExactly<InvalidOperationException>(() =>
        {
            using var publisher = new AmqpEventPublisher(Options.Create(options), new RecordingLogger<AmqpEventPublisher>());
        });
        options.Tls.CaFile = string.Empty;
        options.Tls.CertFile = certificates.CertificatePath;
        options.Tls.KeyFile = certificates.WrongKeyPath;
        Assert.ThrowsExactly<InvalidOperationException>(() =>
        {
            using var publisher = new AmqpEventPublisher(Options.Create(options), new RecordingLogger<AmqpEventPublisher>());
        });
    }

    [TestMethod]
    [DataRow("anonymous")]
    [DataRow("plain")]
    [DataRow("external")]
    public async Task LibraryNegotiatesRequestedSaslAndVerifiedMutualTls(string mechanism)
    {
        using var certificates = new CertificateFiles();
        using var listener = new TcpListener(IPAddress.Loopback, 0);
        listener.Start();
        using var deadline = new CancellationTokenSource(TimeSpan.FromSeconds(5));
        var options = ValidOptions();
        options.Auth.Type = mechanism;
        options.Endpoint = (mechanism == "external" ? "amqps" : "amqp") + "://" + listener.LocalEndpoint;
        var variable = "DSCREGISTRY_EVENTING_TEST_" + Guid.NewGuid().ToString("N");
        if (mechanism == "plain")
        {
            options.Auth.Username = "test-user";
            options.Auth.PasswordEnv = variable;
            Environment.SetEnvironmentVariable(variable, "private-test-password");
        }

        if (mechanism == "external")
        {
            options.Tls.CaFile = certificates.CertificatePath;
            options.Tls.CertFile = certificates.CertificatePath;
            options.Tls.KeyFile = certificates.KeyPath;
        }

        try
        {
            var server = ReadSaslAsync(listener, mechanism, mechanism == "external" ? certificates : null, deadline.Token);
            var logger = new RecordingLogger<AmqpEventPublisher>();
            using var publisher = new AmqpEventPublisher(Options.Create(options), logger);
            await Assert.ThrowsExactlyAsync<InvalidOperationException>(() => publisher.PublishAsync(new RegistryEvent("machine.created", "machine"), deadline.Token));
            var sasl = Encoding.ASCII.GetString(await server);
            StringAssert.Contains(sasl, mechanism.ToUpperInvariant(), StringComparison.Ordinal);
            if (mechanism == "plain")
            {
                StringAssert.Contains(sasl, "test-user", StringComparison.Ordinal);
                StringAssert.Contains(sasl, "private-test-password", StringComparison.Ordinal);
            }

            Assert.IsFalse(logger.Messages.Any(message => message.Contains("private-test-password", StringComparison.Ordinal)));
        }

        finally
        {
            Environment.SetEnvironmentVariable(variable, null);
        }
    }

    [TestMethod]
    [DataRow(false)]
    [DataRow(true)]
    public async Task TlsRejectsUntrustedRootOrWrongEndpointHostname(bool trustPrivateRoot)
    {
        using var certificates = new CertificateFiles(includeLoopbackAddress: false);
        using var listener = new TcpListener(IPAddress.Loopback, 0);
        listener.Start();
        using var deadline = new CancellationTokenSource(TimeSpan.FromSeconds(5));
        var options = ValidOptions();
        options.Endpoint = "amqps://" + listener.LocalEndpoint;
        if (trustPrivateRoot)
        {
            options.Tls.CaFile = certificates.CertificatePath;
        }

        var server = ObserveRejectedTlsAsync(listener, certificates.ServerCertificate, deadline.Token);
        using var publisher = new AmqpEventPublisher(Options.Create(options), new RecordingLogger<AmqpEventPublisher>());
        await Assert.ThrowsExactlyAsync<InvalidOperationException>(() => publisher.PublishAsync(new RegistryEvent("machine.created", "machine"), deadline.Token));
        Assert.IsFalse(await server);
    }

    [TestMethod]
    public async Task CancelingBlockedSaslClosesActualSocketAndCompletesLibraryTask()
    {
        using var listener = new TcpListener(IPAddress.Loopback, 0);
        listener.Start();
        using var cancellation = new CancellationTokenSource();
        using var testDeadline = new CancellationTokenSource(TimeSpan.FromSeconds(5));
        var options = ValidOptions();
        options.Endpoint = "amqp://" + listener.LocalEndpoint;
        options.ConnectTimeout = TimeSpan.FromMinutes(1);
        options.PublishTimeout = TimeSpan.FromMinutes(1);
        using var publisher = new AmqpEventPublisher(Options.Create(options), new RecordingLogger<AmqpEventPublisher>());
        var publishing = publisher.PublishAsync(new RegistryEvent("machine.created", "machine"), cancellation.Token);
        using var peer = await listener.AcceptTcpClientAsync(testDeadline.Token);
        var stream = peer.GetStream();
        var header = new byte[8];
        await stream.ReadExactlyAsync(header, testDeadline.Token);
        CollectionAssert.AreEqual(new byte[] { 65, 77, 81, 80, 3, 1, 0, 0 }, header);
        cancellation.Cancel();
        await Assert.ThrowsAsync<OperationCanceledException>(() => publishing.WaitAsync(TimeSpan.FromSeconds(3)));
        await AssertPeerDisconnectedAsync(stream, testDeadline.Token);
    }

    [TestMethod]
    public async Task ConnectDeadlineClosesSocketAndBackoffIsInsideNextAttemptBudget()
    {
        using var listener = new TcpListener(IPAddress.Loopback, 0);
        listener.Start();
        using var deadline = new CancellationTokenSource(TimeSpan.FromSeconds(5));
        var options = ValidOptions();
        options.Endpoint = "amqp://" + listener.LocalEndpoint;
        options.ConnectTimeout = TimeSpan.FromMilliseconds(100);
        options.PublishTimeout = TimeSpan.FromMilliseconds(150);
        using var publisher = new AmqpEventPublisher(Options.Create(options), new RecordingLogger<AmqpEventPublisher>());
        var publishing = publisher.PublishAsync(new RegistryEvent("machine.created", "machine"), deadline.Token);
        using var peer = await listener.AcceptTcpClientAsync(deadline.Token);
        await peer.GetStream().ReadExactlyAsync(new byte[8], deadline.Token);
        await Assert.ThrowsExactlyAsync<InvalidOperationException>(() => publishing);
        await AssertPeerDisconnectedAsync(peer.GetStream(), deadline.Token);
        await Assert.ThrowsAsync<OperationCanceledException>(() => publisher.PublishAsync(new RegistryEvent("machine.created", "subsequent"), deadline.Token));
        Assert.IsFalse(listener.Pending());
    }

    private static EventingOptions ValidOptions()
    {
        return new EventingOptions
        {
            Enabled = true,
            Endpoint = "amqp://127.0.0.1:1",
            Address = "/queues/events"
        };
    }

    private static IConfiguration EnabledConfiguration(Dictionary<string, string?>? overrides = null)
    {
        var values = new Dictionary<string, string?>
        {
            ["Eventing:Enabled"] = "true",
            ["Eventing:Endpoint"] = "amqp://127.0.0.1:1",
            ["Eventing:Address"] = "/queues/events",
            ["Eventing:ConnectTimeout"] = "5s",
            ["Eventing:PublishTimeout"] = "10s",
            ["Eventing:ShutdownTimeout"] = "5s"
        };
        if (overrides != null)
        {
            foreach (var pair in overrides)
            {
                values[pair.Key] = pair.Value;
            }
        }

        return new ConfigurationBuilder().AddInMemoryCollection(values).Build();
    }

    private static EventWorker CreateWorker(
        FakePublisher publisher,
        RecordingLogger<EventWorker> logger,
        int capacity = 2,
        TimeSpan? publishTimeout = null,
        TimeSpan? shutdownTimeout = null)
    {
        var options = ValidOptions();
        options.QueueCapacity = capacity;
        options.PublishTimeout = publishTimeout ?? TimeSpan.FromSeconds(2);
        options.ShutdownTimeout = shutdownTimeout ?? TimeSpan.FromSeconds(2);
        return new EventWorker(publisher, Options.Create(options), logger);
    }

    private static async Task<byte[]> ReadSaslAsync(TcpListener listener, string mechanism, CertificateFiles? certificates, CancellationToken token)
    {
        using var peer = await listener.AcceptTcpClientAsync(token);
        Stream stream = peer.GetStream();
        if (certificates != null)
        {
            var secure = new SslStream(stream, leaveInnerStreamOpen: false, (sender, certificate, chain, errors) =>
            {
                if (certificate == null)
                {
                    return false;
                }

                using var validation = new X509Chain();
                validation.ChainPolicy.TrustMode = X509ChainTrustMode.CustomRootTrust;
                validation.ChainPolicy.CustomTrustStore.Add(certificates.ServerCertificate);
                validation.ChainPolicy.RevocationMode = X509RevocationMode.NoCheck;
                Assert.AreEqual(typeof(X509Certificate2), certificate.GetType());
                return validation.Build((X509Certificate2)certificate);
            });
            await secure.AuthenticateAsServerAsync(new SslServerAuthenticationOptions
            {
                ServerCertificate = certificates.ServerCertificate,
                ClientCertificateRequired = true,
                EnabledSslProtocols = SslProtocols.Tls12 | SslProtocols.Tls13
            }, token);
            stream = secure;
        }

        await using (stream)
        {
            var header = new byte[8];
            await stream.ReadExactlyAsync(header, token);
            CollectionAssert.AreEqual(new byte[] { 65, 77, 81, 80, 3, 1, 0, 0 }, header);
            await stream.WriteAsync(header, token);
            var name = Encoding.ASCII.GetBytes(mechanism.ToUpperInvariant());
            var body = new List<byte>
                {
                    0, 0x53, 0x40, 0xc0, (byte)(6 + name.Length), 1,
                    0xe0, (byte)(3 + name.Length), 1, 0xa3, (byte)name.Length
                };
            body.AddRange(name);
            var frame = new byte[8 + body.Count];
            System.Buffers.Binary.BinaryPrimitives.WriteUInt32BigEndian(frame, (uint)frame.Length);
            frame[4] = 2;
            frame[5] = 1;
            body.CopyTo(frame, 8);
            await stream.WriteAsync(frame, token);
            await stream.ReadExactlyAsync(header, token);
            var size = System.Buffers.Binary.BinaryPrimitives.ReadUInt32BigEndian(header);
            Assert.IsTrue(size >= 8u && size <= 65536u);
            var sasl = new byte[size - 8];
            await stream.ReadExactlyAsync(sasl, token);
            return sasl;
        }
    }

    private static async Task<bool> ObserveRejectedTlsAsync(TcpListener listener, X509Certificate2 certificate, CancellationToken token)
    {
        using var peer = await listener.AcceptTcpClientAsync(token);
        using var stream = new SslStream(peer.GetStream());
        try
        {
            await stream.AuthenticateAsServerAsync(new SslServerAuthenticationOptions
            {
                ServerCertificate = certificate,
                EnabledSslProtocols = SslProtocols.Tls12 | SslProtocols.Tls13
            }, token);
            var header = new byte[8];
            return await stream.ReadAsync(header, token) != 0;
        }

        catch (Exception exception) when (exception is AuthenticationException or IOException)
        {
            return false;
        }
    }

    private static async Task AssertPeerDisconnectedAsync(NetworkStream stream, CancellationToken token)
    {
        try
        {
            Assert.AreEqual(0, await stream.ReadAsync(new byte[1], token));
        }

        catch (IOException exception) when (exception.InnerException is SocketException socketException &&
            socketException.SocketErrorCode is SocketError.ConnectionReset or SocketError.ConnectionAborted or SocketError.Shutdown)
        {
            // Windows may surface an aborted connection as RST rather than EOF.
        }
    }

    private class FakePublisher : IEventPublisher
    {
        private readonly Channel<RegistryEvent> entered = Channel.CreateUnbounded<RegistryEvent>();
        private readonly Func<RegistryEvent, CancellationToken, Task> behavior;
        private readonly ConcurrentQueue<RegistryEvent> notifications = new ConcurrentQueue<RegistryEvent>();
        private int closeCount;
        private int abortCount;
        private int disposed;

        public FakePublisher(Func<RegistryEvent, CancellationToken, Task>? behavior = null)
        {
            this.behavior = behavior ?? ((notification, token) => Task.CompletedTask);
        }

        public IReadOnlyCollection<RegistryEvent> Notifications
        {
            get
            {
                return this.notifications.ToArray();
            }
        }

        public int CloseCount
        {
            get
            {
                return Volatile.Read(ref this.closeCount);
            }
        }

        public int AbortCount
        {
            get
            {
                return Volatile.Read(ref this.abortCount);
            }
        }

        public Task PublishAsync(RegistryEvent notification, CancellationToken cancellationToken)
        {
            this.notifications.Enqueue(notification);
            this.entered.Writer.TryWrite(notification);
            return this.behavior(notification, cancellationToken);
        }

        public async Task<RegistryEvent> ReadAsync()
        {
            return await this.entered.Reader.ReadAsync().AsTask().WaitAsync(TimeSpan.FromSeconds(3));
        }

        public void Abort()
        {
            Interlocked.Increment(ref this.abortCount);
        }

        public void Dispose()
        {
            if (Interlocked.Exchange(ref this.disposed, 1) == 0)
            {
                Interlocked.Increment(ref this.closeCount);
            }
        }
    }

    private class RecordingLogger<T> : ILogger<T>
    {
        private readonly ConcurrentQueue<string> messages = new ConcurrentQueue<string>();

        public IReadOnlyCollection<string> Messages
        {
            get
            {
                return this.messages.ToArray();
            }
        }

        public IDisposable? BeginScope<TState>(TState state) where TState : notnull
        {
            return null;
        }

        public bool IsEnabled(LogLevel logLevel)
        {
            return true;
        }

        public void Log<TState>(LogLevel logLevel, EventId eventId, TState state, Exception? exception, Func<TState, Exception?, string> formatter)
        {
            Assert.IsNull(exception);
            this.messages.Enqueue(formatter(state, exception));
        }

        public int CountOutcome(string outcome)
        {
            return this.messages.Count(message => message.StartsWith("Event delivery: " + outcome + ";", StringComparison.Ordinal));
        }
    }

    private class SimulatedHttpServer : IHostedService
    {
        private readonly EventDispatcher dispatcher;

        public SimulatedHttpServer(EventDispatcher dispatcher)
        {
            this.dispatcher = dispatcher;
        }

        public Task StartAsync(CancellationToken cancellationToken)
        {
            return Task.CompletedTask;
        }

        public Task StopAsync(CancellationToken cancellationToken)
        {
            this.dispatcher.Notify("machine.created", "http-drain-commit");
            return Task.CompletedTask;
        }
    }

    private class CertificateFiles : IDisposable
    {
        private readonly string directory;

        public CertificateFiles(bool includeLoopbackAddress = true)
        {
            this.directory = Path.Combine(Directory.GetCurrentDirectory(), "eventing-test-" + Guid.NewGuid().ToString("N"));
            Directory.CreateDirectory(this.directory);
            using var key = RSA.Create(2048);
            var request = new CertificateRequest("CN=eventing-test", key, HashAlgorithmName.SHA256, RSASignaturePadding.Pkcs1);
            request.CertificateExtensions.Add(new X509BasicConstraintsExtension(true, false, 0, true));
            request.CertificateExtensions.Add(new X509KeyUsageExtension(X509KeyUsageFlags.DigitalSignature | X509KeyUsageFlags.KeyCertSign, true));
            var usage = new OidCollection
                {
                    new Oid("1.3.6.1.5.5.7.3.1"),
                    new Oid("1.3.6.1.5.5.7.3.2")
                };
            request.CertificateExtensions.Add(new X509EnhancedKeyUsageExtension(usage, true));
            var names = new SubjectAlternativeNameBuilder();
            names.AddDnsName("eventing-test");
            if (includeLoopbackAddress)
            {
                names.AddIpAddress(IPAddress.Loopback);
            }

            request.CertificateExtensions.Add(names.Build());
            using var certificate = request.CreateSelfSigned(DateTimeOffset.UtcNow.AddHours(-1), DateTimeOffset.UtcNow.AddHours(1));
            // Windows Schannel needs a key usable by the TLS service provider.
            this.ServerCertificate = X509CertificateLoader.LoadPkcs12(certificate.Export(X509ContentType.Pfx), null);
            this.CertificatePath = Path.Combine(this.directory, "certificate.pem");
            this.KeyPath = Path.Combine(this.directory, "key.pem");
            this.WrongKeyPath = Path.Combine(this.directory, "wrong-key.pem");
            this.InvalidPemPath = Path.Combine(this.directory, "invalid.pem");
            File.WriteAllText(this.CertificatePath, certificate.ExportCertificatePem());
            File.WriteAllText(this.KeyPath, key.ExportPkcs8PrivateKeyPem());
            using var wrongKey = RSA.Create(2048);
            File.WriteAllText(this.WrongKeyPath, wrongKey.ExportPkcs8PrivateKeyPem());
            File.WriteAllText(this.InvalidPemPath, "not a PEM certificate");
        }

        public X509Certificate2 ServerCertificate { get; }

        public string CertificatePath { get; }

        public string KeyPath { get; }

        public string WrongKeyPath { get; }

        public string InvalidPemPath { get; }

        public void Dispose()
        {
            this.ServerCertificate.Dispose();
            Directory.Delete(this.directory, recursive: true);
        }
    }
}