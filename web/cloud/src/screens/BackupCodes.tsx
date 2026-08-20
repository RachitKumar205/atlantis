import { useState } from 'react'

import { Button, Card, Notice } from '@/components/ui'
import { canLeaveBackupCodes } from '@/lib/flow'

/**
 * The one screen that shows a secret exactly once.
 *
 * The codes are generated, hashed, stored and returned in a single response.
 * There is no route that shows them again — regenerating is the only recovery,
 * and that route does not exist yet. So leaving is deliberate: the checkbox is
 * the gate, and canLeaveBackupCodes is the rule, tested separately.
 */
export function BackupCodes({ codes, signedIn, onDone }: {
  codes: string[]
  signedIn: boolean
  onDone: () => void
}) {
  const [acknowledged, setAcknowledged] = useState(false)

  return (
    <Card
      title="Save your backup codes"
      subtitle="Each one signs you in once, if you lose your authenticator."
    >
      <Notice kind="error">
        These are shown once. They cannot be displayed again.
      </Notice>

      <ul className="codes">
        {codes.map((c) => (
          <li key={c}><code>{c}</code></li>
        ))}
      </ul>

      <label className="check">
        <input
          type="checkbox"
          checked={acknowledged}
          onChange={(e) => setAcknowledged(e.target.checked)}
        />
        <span>I have saved these somewhere safe</span>
      </label>

      <Button onClick={onDone} disabled={!canLeaveBackupCodes(acknowledged)}>
        {signedIn ? 'Continue' : 'Done'}
      </Button>
    </Card>
  )
}
