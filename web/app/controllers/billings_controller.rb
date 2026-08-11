# 契約。
#
# 判定はここに書かない。Go の billing.Evaluate が返したものをそのまま出す。
# 「支払っているのに使えない」も「解約したのに使える」も事故になるので、
# 判定を2箇所に持たせない。
class BillingsController < ApplicationController
  def show
    @billing = billing_status
    @done = params[:done] == "1"
    @canceled = params[:canceled] == "1"
  end

  def checkout
    return deny unless current_user.can_manage_billing?

    res = ApiClient.new.checkout_url(
      organization_id: current_organization.id, actor_email: current_user.email
    )
    if res[:ok]
      # Stripe の画面へ送る。カード番号はこちらを通らない。
      redirect_to res[:url], allow_other_host: true
    else
      redirect_to billing_path, alert: res[:error]
    end
  rescue ApiClient::Unreachable => e
    redirect_to billing_path, alert: "処理サーバーに繋がりません（#{e.message}）"
  end

  def portal
    return deny unless current_user.can_manage_billing?

    res = ApiClient.new.portal_url(organization_id: current_organization.id)
    if res[:ok]
      redirect_to res[:url], allow_other_host: true
    else
      redirect_to billing_path, alert: res[:error]
    end
  rescue ApiClient::Unreachable => e
    redirect_to billing_path, alert: "処理サーバーに繋がりません（#{e.message}）"
  end

  private

  def deny
    redirect_to billing_path, alert: "契約の手続きができるのは管理者だけです"
  end
end
