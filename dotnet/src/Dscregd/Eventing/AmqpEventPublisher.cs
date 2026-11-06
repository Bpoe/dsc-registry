namespace Dscregd.Eventing;

using System;
using System.Diagnostics;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using Amqp;
using Amqp.Framing;
using Amqp.Types;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;

public class AmqpEventPublisher : IEventPublisher
{
    public const string ContentType = "application/cloudevents+json";
    private readonly object gate = new object();
    private readonly EventingOptions options;
    private readonly AmqpTlsSettings tls;
    private readonly ILogger<AmqpEventPublisher> logger;
    private AmqpSocketProvider? socketProvider;
    private Connection? connection;
    private Session? session;
    private SenderLink? sender;
    private TimeSpan backoff;
    private long lastFailure;
    private bool connectedBefore;
    private int disposed;

    public AmqpEventPublisher(IOptions<EventingOptions> options, ILogger<AmqpEventPublisher> logger)
    {
        ArgumentNullException.ThrowIfNull(options);
        ArgumentNullException.ThrowIfNull(logger);
        this.options = options.Value;
        this.options.Validate();
        if (!this.options.Enabled)
        {
            throw new InvalidOperationException("A disabled eventing configuration must not create a publisher.");
        }

        this.logger = logger;
        this.tls = new AmqpTlsSettings(this.options);
    }

    public static Message CreateMessage(RegistryEvent notification)
    {
        ArgumentNullException.ThrowIfNull(notification);
        var properties = new Properties
        {
            ContentType = new Symbol(ContentType)
        };
        // The string convenience property would encode an AMQP string.
        properties.SetMessageId(notification.Id);
        return new Message
        {
            Properties = properties,
            BodySection = new Data
            {
                Binary = JsonSerializer.SerializeToUtf8Bytes(notification)
            }
        };
    }

    public static bool IsAccepted(Outcome? outcome)
    {
        return outcome is Accepted;
    }

    public static TimeSpan NextBackoff(TimeSpan previous)
    {
        return previous == TimeSpan.Zero
            ? TimeSpan.FromMilliseconds(500)
            : TimeSpan.FromMilliseconds(Math.Min(previous.TotalMilliseconds * 2, 30000));
    }

    public async Task PublishAsync(RegistryEvent notification, CancellationToken cancellationToken)
    {
        ObjectDisposedException.ThrowIf(Volatile.Read(ref this.disposed) != 0, this);
        using var attempt = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        attempt.CancelAfter(this.options.PublishTimeout);
        var token = attempt.Token;
        if (this.backoff > TimeSpan.Zero)
        {
            var remaining = this.backoff - Stopwatch.GetElapsedTime(this.lastFailure);
            if (remaining > TimeSpan.Zero)
            {
                await Task.Delay(remaining, token).ConfigureAwait(false);
            }
        }

        token.ThrowIfCancellationRequested();
        try
        {
            SenderLink? sender;
            lock (this.gate)
            {
                sender = this.sender;
            }

            if (sender == null)
            {
                try
                {
                    sender = await this.ConnectAsync(token).ConfigureAwait(false);
                }

                catch
                {
                    this.logger.LogWarning("AMQP connection: {Outcome}", "connect_failed");
                    throw;
                }

                this.logger.LogInformation("AMQP connection: {Outcome}", this.connectedBefore ? "reconnected" : "connected");
                this.connectedBefore = true;
            }

            using var notificationMessage = CreateMessage(notification);
            var receipt = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            try
            {
                sender.Send(
                    notificationMessage,
                    static (link, message, outcome, state) =>
                    {
                        ((TaskCompletionSource<bool>)state).TrySetResult(IsAccepted(outcome));
                    },
                    receipt);
                if (!await receipt.Task.WaitAsync(token).ConfigureAwait(false))
                {
                    throw new InvalidOperationException("AMQP delivery was not accepted.");
                }
            }

            catch
            {
                this.Abort();
                throw;
            }

            this.backoff = TimeSpan.Zero;
            this.lastFailure = 0;
        }

        catch (Exception)
        {
            this.logger.LogWarning("AMQP connection: {Outcome}", "connection_failed");
            this.Abort();
            this.backoff = NextBackoff(this.backoff);
            this.lastFailure = Stopwatch.GetTimestamp();
            if (token.IsCancellationRequested)
            {
                throw new OperationCanceledException(token);
            }

            // No broker-controlled descriptions or credentials escape this boundary.
            throw new InvalidOperationException("AMQP event publication failed.");
        }
    }

    public void Abort()
    {
        AmqpSocketProvider? provider;
        Connection? connection;
        Session? session;
        SenderLink? sender;
        lock (this.gate)
        {
            provider = this.socketProvider;
            connection = this.connection;
            session = this.session;
            sender = this.sender;
            this.socketProvider = null;
            this.connection = null;
            this.session = null;
            this.sender = null;
        }

        try
        {
            provider?.Dispose();
        }

        catch (Exception)
        {
            this.logger.LogWarning("AMQP socket cleanup failed.");
        }

        foreach (var resource in new AmqpObject?[] { sender, session, connection })
        {
            if (resource == null)
            {
                continue;
            }

            try
            {
                // Nonblocking close after socket disposal releases library
                // resources; never use the 60-second peer-handshake defaults.
                resource.Close(TimeSpan.Zero);
            }

            catch (Exception)
            {
                this.logger.LogWarning("AMQP connection cleanup failed.");
            }
        }
    }

    public void Dispose()
    {
        if (Interlocked.Exchange(ref this.disposed, 1) != 0)
        {
            return;
        }

        this.Abort();
        this.tls.Dispose();
    }

    private async Task<SenderLink> ConnectAsync(CancellationToken attemptToken)
    {
        using var deadline = CancellationTokenSource.CreateLinkedTokenSource(attemptToken);
        deadline.CancelAfter(this.options.ConnectTimeout);
        var token = deadline.Token;
        var provider = new AmqpSocketProvider(this.tls, token);
        lock (this.gate)
        {
            ObjectDisposedException.ThrowIf(this.disposed != 0, this);
            this.socketProvider = provider;
        }

        // This registration covers SASL, AMQP Open, Begin and Attach as well
        // as TCP/TLS. WaitAsync alone would abandon a live negotiation socket.
        using var interrupt = token.UnsafeRegister(
            state =>
            {
                try
                {
                    ((AmqpSocketProvider)state!).Dispose();
                }

                catch (Exception)
                {
                    this.logger.LogWarning("AMQP canceled socket cleanup failed.");
                }
            },
            provider);
        var factory = new ConnectionFactory(new[] { provider });
        factory.SASL.Profile = this.tls.CreateSaslProfile();
        factory.AMQP.ContainerId = "dsc-registry-" + Guid.NewGuid().ToString("D");
        var endpoint = this.tls.CreateAddress(this.options.Endpoint);
        var connecting = factory.CreateAsync(endpoint, token);
        Connection connection;
        try
        {
            connection = await connecting.WaitAsync(token).ConfigureAwait(false);
        }

        catch
        {
            provider.Dispose();
            // Socket disposal/cancellation also completes the library task;
            // observe it instead of leaving faulted negotiation tasks behind.
            try
            {
                await connecting.ConfigureAwait(false);
            }

            catch (Exception)
            {
            }

            throw;
        }

        lock (this.gate)
        {
            if (this.socketProvider != provider || token.IsCancellationRequested)
            {
                provider.Dispose();
                connection.Close(TimeSpan.Zero);
                throw new OperationCanceledException(token);
            }

            this.connection = connection;
        }

        var began = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        var session = new Session(
            connection,
            new Begin(),
            (remoteSession, begin) =>
            {
                began.TrySetResult(true);
            });
        session.AddClosedCallback((closed, error) =>
        {
            began.TrySetResult(false);
        });
        lock (this.gate)
        {
            if (this.socketProvider != provider || token.IsCancellationRequested)
            {
                throw new OperationCanceledException(token);
            }

            this.session = session;
        }

        if (!await began.Task.WaitAsync(token).ConfigureAwait(false))
        {
            throw new InvalidOperationException("AMQP session was closed.");
        }

        var attached = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        var sender = new SenderLink(
            session,
            "registry-events-" + Guid.NewGuid().ToString("D"),
            new Attach
            {
                Source = new Source(),
                Target = new Target
                {
                    Address = this.options.Address
                },
                SndSettleMode = SenderSettleMode.Unsettled,
                RcvSettleMode = ReceiverSettleMode.First
            },
            (link, attach) =>
            {
                attached.TrySetResult(attach.Target != null && attach.SndSettleMode != SenderSettleMode.Settled);
            });
        sender.AddClosedCallback((closed, error) =>
        {
            attached.TrySetResult(false);
        });
        if (!await attached.Task.WaitAsync(token).ConfigureAwait(false))
        {
            throw new InvalidOperationException("AMQP destination did not accept an unsettled sender.");
        }

        lock (this.gate)
        {
            if (this.socketProvider != provider || token.IsCancellationRequested)
            {
                throw new OperationCanceledException(token);
            }

            this.sender = sender;
        }

        return sender;
    }
}