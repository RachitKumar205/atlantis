import { enquiry } from "../content";

// initEnquiry sends form to its action as JSON and reports the answer in the
// form's status line.
//
// The endpoint is Cloud's, on another origin, and reads JSON only. Its 400
// answers carry an "error" phrase about the input, shown capitalised with a
// full stop.
export function initEnquiry(form: HTMLFormElement) {
  const button = form.querySelector<HTMLButtonElement>('button[type="submit"]');
  const status = form.querySelector<HTMLElement>("[data-enquiry-status]");
  if (!button || !status) return;
  const label = button.textContent;

  const report = (state: "sent" | "error", text: string) => {
    form.dataset.state = state;
    status.textContent = text;
  };

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const data = new FormData(form);
    const email = String(data.get("email") ?? "").trim();

    delete form.dataset.state;
    status.textContent = "";
    button.disabled = true;
    button.textContent = enquiry.sending;

    try {
      const res = await fetch(form.action, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          email,
          message: String(data.get("message") ?? ""),
          website: String(data.get("website") ?? ""),
        }),
      });
      if (res.ok) {
        form.reset();
        report("sent", enquiry.sent);
        return;
      }
      if (res.status === 429) {
        report("error", enquiry.limited);
        return;
      }
      const body: { error?: string } | null = await res.json().catch(() => null);
      if (res.status === 400 && body?.error) {
        report("error", body.error.charAt(0).toUpperCase() + body.error.slice(1) + ".");
        return;
      }
      report("error", enquiry.failed);
    } catch {
      report("error", enquiry.failed);
    } finally {
      button.disabled = false;
      button.textContent = label;
    }
  });
}
