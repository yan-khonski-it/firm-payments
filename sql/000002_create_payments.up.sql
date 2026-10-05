CREATE TABLE payments
(
    id            INTEGER GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payer_firm_id INTEGER NOT NULL,
    payee_firm_id INTEGER NOT NULL,
    amount_cents  INTEGER NOT NULL,
    description   TEXT    NOT NULL,

    CONSTRAINT payments_payer_firm_fk FOREIGN KEY (payer_firm_id) REFERENCES firms (id),
    CONSTRAINT payments_payee_firm_fk FOREIGN KEY (payee_firm_id) REFERENCES firms (id),
    CONSTRAINT payments_amount_cents_positive CHECK (amount_cents > 0),
    CONSTRAINT payments_description_max_length CHECK (char_length(description) <= 500),
    CONSTRAINT payments_payer_ne_payee CHECK (payer_firm_id <> payee_firm_id)
);
