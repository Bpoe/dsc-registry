namespace Dscregd.Eventing;

using System;
using System.Globalization;
using System.IO;
using System.Linq;
using System.Text.RegularExpressions;
using Microsoft.Extensions.Configuration;

public class EventingOptions
{
    public const string SectionName = "Eventing";
    private static readonly TimeSpan maximumTimeout = TimeSpan.FromMilliseconds(uint.MaxValue - 1);

    public bool Enabled { get; set; }

    public string Type { get; set; } = "amqp1";

    public string Endpoint { get; set; } = string.Empty;

    public string Address { get; set; } = string.Empty;

    public EventingAuthOptions Auth { get; set; } = new EventingAuthOptions();

    public EventingTlsOptions Tls { get; set; } = new EventingTlsOptions();

    public int QueueCapacity { get; set; } = 1000;

    public TimeSpan ConnectTimeout { get; set; } = TimeSpan.FromSeconds(5);

    public TimeSpan PublishTimeout { get; set; } = TimeSpan.FromSeconds(10);

    public TimeSpan ShutdownTimeout { get; set; } = TimeSpan.FromSeconds(5);

    public void Validate()
    {
        if (!this.Enabled)
        {
            return;
        }

        if (this.Type != "amqp1")
        {
            throw new InvalidOperationException("Eventing transport must be AMQP 1.0.");
        }

        if (!Uri.TryCreate(this.Endpoint, UriKind.Absolute, out var endpoint) ||
            (endpoint.Scheme != "amqp" && endpoint.Scheme != "amqps") ||
            string.IsNullOrEmpty(endpoint.Host) ||
            !string.IsNullOrEmpty(endpoint.UserInfo) ||
            this.Endpoint.Contains('@') ||
            !string.IsNullOrEmpty(endpoint.Query) ||
            !string.IsNullOrEmpty(endpoint.Fragment) ||
            this.Endpoint.Contains('?') ||
            this.Endpoint.Contains('#') ||
            (endpoint.AbsolutePath != string.Empty && endpoint.AbsolutePath != "/") ||
            this.Endpoint != this.Endpoint.Trim() ||
            this.Endpoint.EndsWith(':') ||
            this.Endpoint.EndsWith(":/", StringComparison.Ordinal) ||
            endpoint.Authority.EndsWith(':') ||
            endpoint.Port == 0 ||
            endpoint.Port > 65535)
        {
            throw new InvalidOperationException("Eventing endpoint requires an AMQP host without credentials, destination path, query or fragment.");
        }

        if (string.IsNullOrWhiteSpace(this.Address))
        {
            throw new InvalidOperationException("Eventing destination address is required.");
        }

        if (this.QueueCapacity < 1 || this.QueueCapacity > 1000000)
        {
            throw new InvalidOperationException("Eventing queue capacity must be between 1 and 1000000.");
        }

        if (!ValidTimeout(this.ConnectTimeout) || !ValidTimeout(this.PublishTimeout) || !ValidTimeout(this.ShutdownTimeout))
        {
            throw new InvalidOperationException("Eventing timeouts must be positive and supported by the system timer.");
        }

        if (this.Auth == null || this.Tls == null)
        {
            throw new InvalidOperationException("Eventing authentication and TLS settings must be objects.");
        }

        var hasCertificate = !string.IsNullOrEmpty(this.Tls.CertFile);
        var hasKey = !string.IsNullOrEmpty(this.Tls.KeyFile);
        if (hasCertificate != hasKey)
        {
            throw new InvalidOperationException("Eventing TLS client certificate and key must both be specified.");
        }

        if (endpoint.Scheme != "amqps" && (hasCertificate || !string.IsNullOrEmpty(this.Tls.CaFile) || this.Auth.Type == "external"))
        {
            throw new InvalidOperationException("Eventing TLS settings and EXTERNAL authentication require AMQPS.");
        }

        switch (this.Auth.Type)
        {
            case "plain":
                if (string.IsNullOrEmpty(this.Auth.Username) || string.IsNullOrEmpty(this.Auth.PasswordEnv))
                {
                    throw new InvalidOperationException("Eventing PLAIN authentication requires a username and password environment variable.");
                }

                break;
            case "anonymous":
            case "external":
                if (!string.IsNullOrEmpty(this.Auth.Username) || !string.IsNullOrEmpty(this.Auth.PasswordEnv))
                {
                    throw new InvalidOperationException("Eventing credentials require PLAIN authentication.");
                }

                if (this.Auth.Type == "external" && !hasCertificate)
                {
                    throw new InvalidOperationException("Eventing EXTERNAL authentication requires a TLS client certificate and key.");
                }

                break;
            default:
                throw new InvalidOperationException("Eventing authentication must be anonymous, plain or external.");
        }
    }

    internal static EventingOptions Read(IConfiguration configuration)
    {
        try
        {
            var section = configuration.GetSection(SectionName);
            var options = new EventingOptions
            {
                Enabled = section.GetValue<bool>(nameof(Enabled))
            };
            if (!options.Enabled)
            {
                return options;
            }

            options.Type = section[nameof(Type)] ?? options.Type;
            options.Endpoint = section[nameof(Endpoint)] ?? options.Endpoint;
            options.Address = section[nameof(Address)] ?? options.Address;
            options.QueueCapacity = section.GetValue(nameof(QueueCapacity), options.QueueCapacity);
            options.ConnectTimeout = ReadDuration(section[nameof(ConnectTimeout)], options.ConnectTimeout);
            options.PublishTimeout = ReadDuration(section[nameof(PublishTimeout)], options.PublishTimeout);
            options.ShutdownTimeout = ReadDuration(section[nameof(ShutdownTimeout)], options.ShutdownTimeout);
            section.GetSection(nameof(Auth)).Bind(options.Auth);
            section.GetSection(nameof(Tls)).Bind(options.Tls);
            options.Tls.CaFile = ResolveCertificatePath(configuration, nameof(EventingTlsOptions.CaFile), options.Tls.CaFile);
            options.Tls.CertFile = ResolveCertificatePath(configuration, nameof(EventingTlsOptions.CertFile), options.Tls.CertFile);
            options.Tls.KeyFile = ResolveCertificatePath(configuration, nameof(EventingTlsOptions.KeyFile), options.Tls.KeyFile);
            options.Validate();
            return options;
        }

        catch (Exception exception) when (exception is InvalidOperationException or FormatException or OverflowException or ArgumentException)
        {
            throw new InvalidOperationException("Invalid eventing configuration.");
        }
    }

    private static bool ValidTimeout(TimeSpan timeout)
    {
        return timeout > TimeSpan.Zero && timeout <= maximumTimeout;
    }

    private static string ResolveCertificatePath(IConfiguration configuration, string name, string path)
    {
        if (string.IsNullOrEmpty(path) || Path.IsPathRooted(path) || configuration is not IConfigurationRoot root)
        {
            return path;
        }

        var key = SectionName + ":" + nameof(Tls) + ":" + name;
        foreach (var provider in root.Providers.Reverse())
        {
            if (!provider.TryGet(key, out _))
            {
                continue;
            }

            if (provider is FileConfigurationProvider fileProvider)
            {
                var file = fileProvider.Source.FileProvider?.GetFileInfo(fileProvider.Source.Path ?? string.Empty).PhysicalPath;
                if (file != null)
                {
                    return Path.GetFullPath(path, Path.GetDirectoryName(file)!);
                }
            }

            break;
        }

        return path;
    }

    private static TimeSpan ReadDuration(string? value, TimeSpan defaultValue)
    {
        if (value == null)
        {
            return defaultValue;
        }

        if (TimeSpan.TryParse(value, CultureInfo.InvariantCulture, out var duration))
        {
            return duration;
        }

        // Accept the original Go duration spelling as well as .NET TimeSpan values.
        var matches = Regex.Matches(value, @"\G(\d+(?:\.\d+)?)(ns|us|µs|μs|ms|s|m|h)", RegexOptions.CultureInvariant);
        var length = 0;
        var ticks = 0m;
        foreach (Match match in matches)
        {
            var multiplier = match.Groups[2].Value switch
            {
                "h" => TimeSpan.TicksPerHour,
                "m" => TimeSpan.TicksPerMinute,
                "s" => TimeSpan.TicksPerSecond,
                "ms" => TimeSpan.TicksPerMillisecond,
                "us" or "µs" or "μs" => 10m,
                _ => 0.01m
            };
            ticks += decimal.Parse(match.Groups[1].Value, CultureInfo.InvariantCulture) * multiplier;
            length += match.Length;
        }

        if (length == 0 || length != value.Length)
        {
            throw new FormatException("Invalid eventing duration.");
        }

        return TimeSpan.FromTicks(checked((long)ticks));
    }
}