namespace Dscregd.Eventing;

using System;
using System.IO;
using System.Net.Security;
using System.Security.Authentication;
using System.Security.Cryptography;
using System.Security.Cryptography.X509Certificates;
using Amqp;
using Amqp.Sasl;

internal class AmqpTlsSettings : IDisposable
{
    private readonly X509Certificate2Collection certificates = new X509Certificate2Collection();
    private readonly X509Certificate2Collection roots = new X509Certificate2Collection();
    private readonly string authType;
    private readonly string username;
    private readonly string? password;
    private readonly X509Certificate2? clientCertificate;
    private readonly SslStreamCertificateContext? clientContext;
    private bool disposed;

    public AmqpTlsSettings(EventingOptions options)
    {
        options.Validate();
        this.authType = options.Auth.Type;
        this.username = options.Auth.Username;
        if (this.authType == "plain")
        {
            try
            {
                this.password = Environment.GetEnvironmentVariable(options.Auth.PasswordEnv);
            }

            catch (Exception)
            {
                throw new InvalidOperationException("Cannot read eventing password environment variable.");
            }

            if (string.IsNullOrEmpty(this.password))
            {
                throw new InvalidOperationException("Eventing password environment variable is unset or empty.");
            }
        }

        try
        {
            if (!string.IsNullOrEmpty(options.Tls.CaFile))
            {
                this.roots.ImportFromPem(File.ReadAllText(options.Tls.CaFile));
                if (this.roots.Count == 0)
                {
                    throw new CryptographicException();
                }

                foreach (var certificate in this.roots)
                {
                    if (certificate.Extensions["2.5.29.19"] is not X509BasicConstraintsExtension constraints || !constraints.CertificateAuthority)
                    {
                        throw new CryptographicException();
                    }
                }

                // CustomRootTrust replaces platform trust. Explicitly append
                // both platform stores so private roots do not replace it.
                this.AppendSystemRoots(StoreLocation.LocalMachine);
                this.AppendSystemRoots(StoreLocation.CurrentUser);
            }

            if (!string.IsNullOrEmpty(options.Tls.CertFile))
            {
                using var pemCertificate = X509Certificate2.CreateFromPemFile(options.Tls.CertFile, options.Tls.KeyFile);
                this.clientCertificate = X509CertificateLoader.LoadPkcs12(
                    pemCertificate.Export(X509ContentType.Pfx),
                    null,
                    OperatingSystem.IsWindows() ? X509KeyStorageFlags.DefaultKeySet : X509KeyStorageFlags.EphemeralKeySet);
                if (!this.clientCertificate.HasPrivateKey ||
                    this.clientCertificate.NotBefore.ToUniversalTime() > DateTime.UtcNow ||
                    this.clientCertificate.NotAfter.ToUniversalTime() <= DateTime.UtcNow)
                {
                    throw new CryptographicException();
                }

                this.certificates.ImportFromPem(File.ReadAllText(options.Tls.CertFile));
                this.clientContext = SslStreamCertificateContext.Create(this.clientCertificate, this.certificates, offline: true);
            }
        }

        catch (Exception)
        {
            this.Dispose();
            throw new InvalidOperationException("Cannot load eventing TLS certificate authorities or client certificate and key.");
        }
    }

    public Address CreateAddress(string endpoint)
    {
        var parsed = new Uri(endpoint);
        var port = parsed.Port == -1 ? (parsed.Scheme == "amqps" ? 5671 : 5672) : parsed.Port;
        // PLAIN's profile is internal in AMQPNetLite. The component overload
        // avoids putting credentials into any URI or diagnostic string.
        return new Address(
            parsed.DnsSafeHost,
            port,
            this.authType == "plain" ? this.username : null,
            this.authType == "plain" ? this.password : null,
            null,
            parsed.Scheme);
    }

    public SaslProfile? CreateSaslProfile()
    {
        return this.authType switch
        {
            "plain" => null,
            "external" => SaslProfile.External,
            _ => SaslProfile.Anonymous
        };
    }

    public SslClientAuthenticationOptions CreateClientOptions(string host)
    {
        var options = new SslClientAuthenticationOptions
        {
            TargetHost = host,
            EnabledSslProtocols = SslProtocols.Tls12 | SslProtocols.Tls13,
            CertificateRevocationCheckMode = X509RevocationMode.NoCheck,
            ClientCertificateContext = this.clientContext
        };
        if (this.roots.Count != 0)
        {
            var policy = new X509ChainPolicy
            {
                TrustMode = X509ChainTrustMode.CustomRootTrust,
                VerificationFlags = X509VerificationFlags.NoFlag,
                RevocationMode = X509RevocationMode.NoCheck,
                DisableCertificateDownloads = true
            };
            policy.CustomTrustStore.AddRange(this.roots);
            policy.ApplicationPolicy.Add(new Oid("1.3.6.1.5.5.7.3.1"));
            options.CertificateChainPolicy = policy;
        }

        // No custom certificate callback: SslStream still verifies the
        // endpoint hostname, EKU, lifetime and complete trust chain.
        return options;
    }

    public void Dispose()
    {
        if (this.disposed)
        {
            return;
        }

        this.disposed = true;
        this.clientCertificate?.Dispose();
        foreach (var certificate in this.certificates)
        {
            certificate.Dispose();
        }

        foreach (var certificate in this.roots)
        {
            certificate.Dispose();
        }
    }

    private void AppendSystemRoots(StoreLocation location)
    {
        using var store = new X509Store(StoreName.Root, location);
        store.Open(OpenFlags.ReadOnly);
        this.roots.AddRange(store.Certificates);
    }
}