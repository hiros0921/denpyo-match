# インボイス登録番号の一括照合。
#
# 2023年10月から、受け取った請求書・領収書に「T＋13桁」の登録番号が
# 正しく記載されているかを確かめる必要が出た。番号が無効だと
# 仕入税額控除が取れない。つまり払う税額が変わる。
#
# 受領した帳票にしか発生せず、全件やらなければならず、純粋に機械的で、
# 間違えると税額に直結する。実務では完全に手作業になっている。
#
# この画面が答えるのは1つだけ。「先方に問い合わせるのはどれか」。
class InvoiceRegsController < ApplicationController
  def index
    @client_id = params[:client_id].presence
    @only = params[:only].presence || "attention"
    q = InvoiceRegQuery.new(current_organization.id,
                            client_id: @client_id, only: @only)
    @counts = q.counts
    @total = q.total
    @rows = q.rows
    # 同じ番号が別々の取引先名で出ていないか。書き写し間違いはこう現れる。
    @conflicts = q.conflicts
  end
end
