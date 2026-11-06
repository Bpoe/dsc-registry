FROM mcr.microsoft.com/dotnet/sdk:10.0 AS build
WORKDIR /src
COPY dotnet/ dotnet/
COPY migrations/ migrations/
RUN dotnet publish dotnet/src/Dscregd/Dscregd.csproj -c Release -o /out

FROM mcr.microsoft.com/dotnet/aspnet:10.0
WORKDIR /app
COPY --from=build /out/ ./
USER $APP_UID
EXPOSE 8080
ENV Kestrel__Endpoints__Http__Url=http://0.0.0.0:8080
ENV Registry__Database__ConnectionString="Data Source=/data/registry.db"
ENV Registry__ReportRoot=/reports
ENTRYPOINT ["dotnet", "dscregd.dll"]
