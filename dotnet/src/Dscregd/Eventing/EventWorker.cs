namespace Dscregd.Eventing;

using System;
using System.Threading;
using System.Threading.Channels;
using System.Threading.Tasks;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;

public class EventWorker : BackgroundService
{
    private readonly object admissionLock = new object();
    private readonly Channel<RegistryEvent> queue;
    private readonly CancellationTokenSource abort = new CancellationTokenSource();
    private readonly CancellationToken abortToken;
    private readonly IEventPublisher publisher;
    private readonly EventingOptions options;
    private readonly ILogger<EventWorker> logger;
    private Task? stopTask;
    private bool admissionClosed;
    private int disposed;
    private int publisherClosed;

    public EventWorker(IEventPublisher publisher, IOptions<EventingOptions> options, ILogger<EventWorker> logger)
    {
        ArgumentNullException.ThrowIfNull(publisher);
        ArgumentNullException.ThrowIfNull(options);
        ArgumentNullException.ThrowIfNull(logger);
        this.options = options.Value;
        this.options.Validate();
        if (!this.options.Enabled)
        {
            throw new InvalidOperationException("A disabled eventing configuration must not create a worker.");
        }

        this.publisher = publisher;
        this.logger = logger;
        this.abortToken = this.abort.Token;
        this.queue = Channel.CreateBounded<RegistryEvent>(new BoundedChannelOptions(this.options.QueueCapacity)
        {
            SingleReader = true,
            SingleWriter = false,
            AllowSynchronousContinuations = false,
            FullMode = BoundedChannelFullMode.Wait
        });
    }

    public override Task StopAsync(CancellationToken cancellationToken)
    {
        // The HTTP hosted service must stop before this method is called.
        // ApplicationStopping alone deliberately does not close admission.
        lock (this.admissionLock)
        {
            if (this.stopTask != null)
            {
                return this.stopTask;
            }

            this.admissionClosed = true;
            this.queue.Writer.TryComplete();
            this.stopTask = this.DrainAsync(cancellationToken);
            return this.stopTask;
        }
    }

    public override void Dispose()
    {
        if (Interlocked.Exchange(ref this.disposed, 1) != 0)
        {
            return;
        }

        lock (this.admissionLock)
        {
            this.admissionClosed = true;
            this.queue.Writer.TryComplete();
        }

        this.abort.Cancel();
        this.AbortPublisher();
        base.Dispose();
        this.abort.Dispose();
    }

    internal void Notify(string type, string machineId, string? configurationName, string? reportId)
    {
        RegistryEvent notification;
        bool queued;
        lock (this.admissionLock)
        {
            if (this.admissionClosed)
            {
                return;
            }

            notification = new RegistryEvent(type, machineId, configurationName, reportId);
            // Wait mode + TryWrite returns false rather than silently evicting
            // an older item or reporting a dropped write as successful.
            queued = this.queue.Writer.TryWrite(notification);
        }

        this.LogDelivery(queued ? LogLevel.Information : LogLevel.Warning, queued ? "queued" : "queue_full", notification);
    }

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        using var lifetime = CancellationTokenSource.CreateLinkedTokenSource(stoppingToken, this.abortToken);
        try
        {
            while (await this.queue.Reader.WaitToReadAsync(lifetime.Token).ConfigureAwait(false))
            {
                lifetime.Token.ThrowIfCancellationRequested();
                if (!this.queue.Reader.TryRead(out var notification))
                {
                    continue;
                }

                using var attempt = CancellationTokenSource.CreateLinkedTokenSource(lifetime.Token);
                attempt.CancelAfter(this.options.PublishTimeout);
                try
                {
                    var delivery = this.publisher.PublishAsync(notification, attempt.Token);
                    try
                    {
                        await delivery.WaitAsync(attempt.Token).ConfigureAwait(false);
                    }

                    catch
                    {
                        this.AbortPublisher();
                        // Abort releases native I/O; join cleanup before the
                        // next attempt can acquire a replacement connection.
                        try
                        {
                            await delivery.ConfigureAwait(false);
                        }

                        catch (Exception)
                        {
                        }

                        throw;
                    }

                    this.LogDelivery(LogLevel.Information, "published", notification);
                }

                catch (Exception)
                {
                    // Broker error text and event bodies are never diagnostic fields.
                    this.LogDelivery(LogLevel.Warning, "publish_failed", notification);
                }
            }
        }

        catch (OperationCanceledException) when (lifetime.IsCancellationRequested)
        {
        }

        finally
        {
            this.ClosePublisher();
        }
    }

    private async Task DrainAsync(CancellationToken cancellationToken)
    {
        using var deadline = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        deadline.CancelAfter(this.options.ShutdownTimeout);
        var execution = this.ExecuteTask;
        if (execution == null)
        {
            this.DiscardPending();
            this.ClosePublisher();
            return;
        }

        try
        {
            await execution.WaitAsync(deadline.Token).ConfigureAwait(false);
        }

        catch (OperationCanceledException) when (deadline.IsCancellationRequested)
        {
            this.abort.Cancel();
            this.AbortPublisher();
            await execution.ConfigureAwait(false);
            var discarded = this.DiscardPending();
            this.logger.LogWarning("Event shutdown: {Outcome}; pending discarded: {Pending}", "timeout", discarded);
        }

        await base.StopAsync(CancellationToken.None).ConfigureAwait(false);
    }

    private int DiscardPending()
    {
        var discarded = 0;
        while (this.queue.Reader.TryRead(out _))
        {
            discarded++;
        }

        return discarded;
    }

    private void AbortPublisher()
    {
        try
        {
            this.publisher.Abort();
        }

        catch (Exception)
        {
            this.logger.LogWarning("Event publisher transport cleanup failed.");
        }
    }

    private void ClosePublisher()
    {
        if (Interlocked.Exchange(ref this.publisherClosed, 1) != 0)
        {
            return;
        }

        try
        {
            this.publisher.Dispose();
        }

        catch (Exception)
        {
            this.logger.LogWarning("Event publisher close failed.");
        }
    }

    private void LogDelivery(LogLevel level, string outcome, RegistryEvent notification)
    {
        this.logger.Log(
            level,
            "Event delivery: {Outcome}; type: {EventType}; id: {EventId}; subject: {Subject}",
            outcome,
            notification.Type,
            notification.Id,
            notification.Subject);
    }
}