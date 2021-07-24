# Data Exchanges 

Exchanges are internal hooks to external apis, to either push data and update an external service, or pull data and update itself from some external service.

Example, use exchange to sync data creation call to Google Sheets. So on every row created using the POST API also creates a corresponding row in your google sheet.

!!! example "Google drive exchange YAML"
    ```yaml
    Exchanges:
    - Name: Task to excel sheet
      SourceAttributes:
        Name: todo
      SourceType: self
      TargetAttributes:
        sheetUrl: https://content-sheets.googleapis.com/v4/spreadsheets/1Ru-bDk3AjQotQj72k8SyxoOs84eXA1Y6sSPumBb3WSA/values/A1:append
        appKey: AIzaSyAC2xame4NShrzH9ZJeEpWT5GkySooa0XM
      TargetType: gsheet-append
      Attributes:
      - SourceColumn: "$self.description"
        TargetColumn: Task description
      - SourceColumn: self.schedule
        TargetColumn: Scheduled at
      Options:
        hasHeader: true
    ```

```yaml
Exchanges:
- Name: Blog to excel sheet sync
  SourceAttributes:
    Name: blog
  SourceType: self
  TargetAttributes:
    sheetUrl: https://content-sheets.googleapis.com/v4/spreadsheets/1Ru-bDk3AjQotQj72k8SyxoOs84eXA1Y6sSPumBb3WSA/values/A1:append
  TargetType: gsheet-append
  Attributes:
  - SourceColumn: "$blog.title"
    TargetColumn: Blog title
  - SourceColumn: "$blog.view_count"
    TargetColumn: View count
  Options:
    hasHeader: true
```


```yaml
Exchanges:
- Name: Blog to excel sheet sync
  SourceAttributes:
    Name: blog
  SourceType: table
  TargetAttributes:
    sheetUrl: https://content-sheets.googleapis.com/v4/spreadsheets/1Ru-bDk3AjQotQj72k8SyxoOs84eXA1Y6sSPumBb3WSA/values/A1:append
  TargetType: gsheet-append
  Attributes:
  - SourceColumn: "$blog.title"
    TargetColumn: Blog title
  - SourceColumn: "$blog.view_count"
    TargetColumn: View count
  Options:
    hasHeader: true
```

