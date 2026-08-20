import { Card, LinkButton } from '@/components/ui'

/**
 * A dead-stop with a way back.
 *
 * Used for the two "check your email" answers. Both routes deliberately reply
 * identically whether or not the address has an account — that is what stops
 * them being a way to find out who has one — so this screen must not say
 * anything the response did not.
 */
export function Message({ title, body, backLabel, onBack }: {
  title: string
  body: string
  backLabel: string
  onBack: () => void
}) {
  return (
    <Card title={title} subtitle={body}>
      <p className="foot">
        <LinkButton onClick={onBack}>{backLabel}</LinkButton>
      </p>
    </Card>
  )
}
