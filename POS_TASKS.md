# Интеграция POS-задач в CRM

Контракт между CRM и `mirai-agent` для карточной оплаты через POS-терминал
ПриватБанка.

## Устройство

Устройство должно иметь тип `pos_terminal`. Один `secretToken` соответствует
одному физическому терминалу.

## Входящая задача

Агент получает задачу `purchase` через `GET /api/v1/devices/tasks`:

```json
{
  "tasks": [
    {
      "id": 1024,
      "name": "purchase",
      "data": {
        "amountMinor": 12345,
        "merchantId": "0"
      },
      "priority": 10,
      "createdAt": "2026-07-14T18:00:00Z"
    }
  ]
}
```

- `amountMinor` — обязательное положительное целое число копеек:
  `12345` = `123.45 UAH`.
- `merchantId` — необязательная строка; пустое или отсутствующее значение
  преобразуется в `"0"`.
- Неизвестные поля в `data`, дробная сумма и дополнительные JSON-значения не
  допускаются.

CRM должна сохранять исходный `task.id` при повторной выдаче задачи. Нельзя
автоматически создавать новый task ID для неопределённого платежа: терминал не
поддерживает idempotency key.

## Выходные данные

Агент отправляет результат в `POST /api/v1/devices/tasks/finalize`:

```json
{
  "tasks": [
    {
      "id": 1024,
      "data": {
        "amountMinor": 12345,
        "merchantId": "0",
        "payment": {
          "status": "approved",
          "requestSent": true,
          "stage": "completed",
          "response": {
            "method": "Purchase",
            "step": 0,
            "params": {
              "amount": "123.45",
              "approvalCode": "999999",
              "invoiceNumber": "000123",
              "merchant": "S1XXXXXX",
              "pan": "4731XXXXXXXX9838",
              "receipt": "text-of-receipt",
              "responseCode": "0000",
              "rrn": "999999999999",
              "terminalId": "S1XXXXXX",
              "date": "14.07.2026",
              "time": "18:03:12",
              "bankAcquirer": "ПриватБанк",
              "paymentSystem": "VISA"
            },
            "error": false,
            "errorDescription": ""
          }
        }
      }
    }
  ]
}
```

`amountMinor` и `merchantId` — нормализованный исходный запрос. Фактически
одобренную сумму нужно брать из `payment.response.params.amount`.

### Статусы

- `approved` — `responseCode = "0000"`.
- `partial` — `responseCode = "0010"`; терминал одобрил только сумму из
  `response.params.amount`.
- `declined` — получен окончательный ответ с другим кодом. Это результат
  платежа, а не ошибка task.
- `unknown` — окончательный ответ не получен либо восстановлена незавершённая
  операция. Платёж мог пройти; автоматически повторять его нельзя.

### `requestSent`

`true` означает, что отправка Purchase была начата или могла начаться до сбоя.
Это не подтверждение списания. При `unknown` требуется сверка с терминалом или
банком.

### `stage`

Диагностическое поле, на котором CRM не должна строить бизнес-логику:

- `connect`
- `handshake`
- `identify`
- `before_send`
- `write_request`
- `await_response`
- `completed`
- `closed`
- `journal_incomplete`
- `recovered_after_restart`

### Ответ терминала

`payment.response` присутствует для `approved`, `partial` и `declined` и
содержит исходный JSON терминала, включая неизвестные агенту поля. Для
`unknown` обычно отсутствует.

Перед отправкой в CRM рекурсивно и без учёта регистра удаляются:

- `track1`
- `cardHolderName`
- `cardExpiryDate`

Остальные поля сохраняются. `receipt` и маскированный `pan` следует защищать и
не выводить в общие application logs.

## Пример отказа

```json
{
  "amountMinor": 12345,
  "merchantId": "0",
  "payment": {
    "status": "declined",
    "requestSent": true,
    "stage": "completed",
    "response": {
      "method": "Purchase",
      "step": 0,
      "params": {
        "responseCode": "0005",
        "receipt": "DECLINED RECEIPT"
      },
      "error": true,
      "errorDescription": "ВІДХИЛЕНО"
    }
  }
}
```

## Пример неопределённого результата

```json
{
  "amountMinor": 12345,
  "merchantId": "0",
  "payment": {
    "status": "unknown",
    "requestSent": true,
    "stage": "await_response",
    "errorDescription": "await purchase response: connection reset"
  }
}
```

После восстановления журнала `stage` равен `recovered_after_restart`.

## Некорректная задача

Только ошибки структуры и неподдерживаемые задачи завершаются через
`error_message`:

```json
{
  "tasks": [
    {
      "id": 1024,
      "error_message": "purchase: amountMinor must be positive"
    }
  ]
}
```

`declined`, `partial` и `unknown` всегда находятся в `data.payment`.

## Ответ CRM на finalize

```json
{
  "finalized": [1024],
  "skipped": []
}
```

Endpoint должен быть идемпотентным по task ID:

- `finalized` — результат принят;
- `skipped` — задача уже была финализирована;
- если task ID отсутствует в обоих массивах, агент сохраняет результат и
  повторяет finalize.

Запись локального журнала `<configPath>.payments.json` удаляется только после
получения task ID в `finalized` или `skipped`. Агент повторяет только finalize,
а не Purchase.

## Логика CRM

1. Связать уникальный task ID с заказом.
2. На `approved` зафиксировать сумму из ответа терминала.
3. На `partial` зафиксировать реально одобренную сумму и передать оператору
   сценарий доплаты или коррекции.
4. На `declined` показать банковский код; новую попытку создавать только явным
   действием оператора и с новым task ID.
5. На `unknown` запретить автоматический retry и выполнить сверку по RRN,
   номеру чека, терминалу или банковскому кабинету.
6. Всегда возвращать подтверждённый task ID в `finalized` или `skipped`.

## Не поддерживается

- возврат (`Refund`);
- отмена (`Withdrawal`, `WithdrawalPartly`);
- USB/COM;
- genericDriverJson WebSocket;
- автоматическое продолжение partial approval.
